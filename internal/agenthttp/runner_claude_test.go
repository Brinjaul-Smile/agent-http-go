package agenthttp

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunClaudeContextCancelsFakeClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake command is POSIX-only")
	}

	workspaceRoot := t.TempDir()
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "claude", "#!/bin/sh\nsleep 10\n")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := RunClaudeContext(ctx, RunRequest{Prompt: "hello", Cwd: workspaceRoot}, RunnerOptions{
		WorkspaceRoot: workspaceRoot,
		Env:           envWithPath(binDir),
		Timeout:       5 * time.Second,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
}

func TestRunClaudeExecutesFakeClaudeAndReturnsJSONResultOutput(t *testing.T) {
	workspaceRoot := t.TempDir()
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "claude", "#!/bin/sh\nprompt=$(cat)\nprintf '{\"result\":\"final:%s\"}' \"$prompt\"\nprintf 'stderr text' >&2\n")

	result, err := RunClaude(RunRequest{Prompt: "hello", Cwd: workspaceRoot}, RunnerOptions{
		WorkspaceRoot: workspaceRoot,
		Env:           envWithPath(binDir),
		Timeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !result.OK {
		t.Fatalf("ok = false, error = %q", result.Error)
	}
	if result.Output != "final:hello" {
		t.Fatalf("output = %q", result.Output)
	}
	if result.Stdout != `{"result":"final:hello"}` {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	if result.Stderr != "stderr text" {
		t.Fatalf("stderr = %q", result.Stderr)
	}
}

func TestRunClaudeStreamIncludesVerboseAndEmitsDeltas(t *testing.T) {
	workspaceRoot := t.TempDir()
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "claude", fakeClaudeStreamScript())

	writer := &collectStreamWriter{}
	result, err := RunClaudeStreamContext(context.Background(), RunRequest{Prompt: "hello", Cwd: workspaceRoot}, writer, RunnerOptions{
		WorkspaceRoot: workspaceRoot,
		Env:           envWithPath(binDir),
		Timeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !result.OK {
		t.Fatalf("ok = false, error = %q, stderr = %q", result.Error, result.Stderr)
	}
	if result.Output != "final:hello" {
		t.Fatalf("output = %q, want final:hello", result.Output)
	}
	if strings.Join(writer.deltas, "") != "stream:hello" {
		t.Fatalf("deltas = %#v, want stream:hello", writer.deltas)
	}
}

func TestClaudeJSONLDeltaParserDoesNotRepeatFinalAssistantMessageAfterExplicitDeltas(t *testing.T) {
	parser := newClaudeJSONLDeltaParser()

	lines := []string{
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"请"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"随时"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"告知"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"请随时告知"}]}}`,
	}

	var deltas []string
	for _, line := range lines {
		deltas = append(deltas, parser.Deltas(line)...)
	}

	if got := strings.Join(deltas, ""); got != "请随时告知" {
		t.Fatalf("deltas = %#v, joined = %q, want 请随时告知", deltas, got)
	}
	if len(deltas) != 3 {
		t.Fatalf("deltas = %#v, want only the three explicit deltas", deltas)
	}
}

func TestClaudeJSONLDeltaParserConvertsCumulativeExplicitDeltaToSuffix(t *testing.T) {
	parser := newClaudeJSONLDeltaParser()

	lines := []string{
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"抱歉，"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"抱歉，查询天气需要发起网络请求"}}`,
	}

	var deltas []string
	for _, line := range lines {
		deltas = append(deltas, parser.Deltas(line)...)
	}

	if got := strings.Join(deltas, ""); got != "抱歉，查询天气需要发起网络请求" {
		t.Fatalf("deltas = %#v, joined = %q", deltas, got)
	}
	if len(deltas) != 2 {
		t.Fatalf("deltas = %#v, want original prefix plus suffix only", deltas)
	}
	if deltas[1] != "查询天气需要发起网络请求" {
		t.Fatalf("second delta = %q, want suffix only", deltas[1])
	}
}

// TestClaudeJSONLDeltaParserSnapshotArrivesBeforeDeltas 验证：快照先于 delta 到达时，
// 快照内容正常发出；随后到来的 delta 不会重复发送快照已有的内容。
func TestClaudeJSONLDeltaParserSnapshotArrivesBeforeDeltas(t *testing.T) {
	parser := newClaudeJSONLDeltaParser()

	lines := []string{
		// 快照先到，包含完整文本
		`{"type":"assistant","message":{"content":[{"type":"text","text":"你好"}]}}`,
		// delta 路接着发来同样的内容（此时快照已更新 lastEmittedText，delta 应被过滤）
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"你好"}}`,
		// 快照再次更新，追加新内容
		`{"type":"assistant","message":{"content":[{"type":"text","text":"你好世界"}]}}`,
	}

	var deltas []string
	for _, line := range lines {
		deltas = append(deltas, parser.Deltas(line)...)
	}

	if got := strings.Join(deltas, ""); got != "你好世界" {
		t.Fatalf("deltas = %#v, joined = %q, want 你好世界", deltas, got)
	}
}

// TestClaudeJSONLDeltaParserSnapshotExtendsAlreadySentDeltas 验证：已通过 delta 路发出
// 部分文本后，快照带来更多内容时，只追加新增部分。
func TestClaudeJSONLDeltaParserSnapshotExtendsAlreadySentDeltas(t *testing.T) {
	parser := newClaudeJSONLDeltaParser()

	lines := []string{
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":", "}}`,
		// 快照包含前两个 delta 已发送的内容，外加新增的 "world!"
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello, world!"}]}}`,
	}

	var deltas []string
	for _, line := range lines {
		deltas = append(deltas, parser.Deltas(line)...)
	}

	if got := strings.Join(deltas, ""); got != "Hello, world!" {
		t.Fatalf("deltas joined = %q, want \"Hello, world!\"", got)
	}
	// 应该是 "Hello" + ", " + "world!" 三段，不含重复全量
	if len(deltas) != 3 {
		t.Fatalf("deltas = %#v, want 3 segments (Hello / ,  / world!)", deltas)
	}
	if deltas[2] != "world!" {
		t.Fatalf("third delta = %q, want \"world!\"", deltas[2])
	}
}

// TestClaudeJSONLDeltaParserStaleSnapshotIsDiscarded 验证：delta 已推进后，
// 姗姗来迟的旧快照（text 比 lastEmittedText 短）应被静默丢弃，
// 不更新 lastEmittedText，不向下游重发任何内容。
func TestClaudeJSONLDeltaParserStaleSnapshotIsDiscarded(t *testing.T) {
	parser := newClaudeJSONLDeltaParser()

	lines := []string{
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":", world"}}`,
		// 旧快照姗姗来迟，只包含第一个 delta 的内容（比 lastEmittedText 短）
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}`,
		// 正常快照，包含完整内容加新增
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello, world!"}]}}`,
	}

	var deltas []string
	for _, line := range lines {
		deltas = append(deltas, parser.Deltas(line)...)
	}

	if got := strings.Join(deltas, ""); got != "Hello, world!" {
		t.Fatalf("deltas joined = %q, want \"Hello, world!\"", got)
	}
	// 旧快照被丢弃，不产生额外 delta；最终应为 Hello / ", world" / "!" 三段
	if len(deltas) != 3 {
		t.Fatalf("deltas = %#v, want 3 segments", deltas)
	}
	if deltas[2] != "!" {
		t.Fatalf("third delta = %q, want \"!\"", deltas[2])
	}
}

func TestClaudeJSONLDeltaParserDoesNotRepeatCorrectedFinalAssistantSnapshot(t *testing.T) {
	parser := newClaudeJSONLDeltaParser()

	lines := []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"我是 Claude。具体来说，"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"通过 Claude Agent SDK 运行。"}}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"我是 Claude。具体来说，我是通过 Claude Agent SDK 运行。"}]}}`,
		`{"type":"result","result":"我是 Claude。具体来说，我是通过 Claude Agent SDK 运行。"}`,
	}

	var deltas []string
	for _, line := range lines {
		deltas = append(deltas, parser.Deltas(line)...)
	}

	if len(deltas) != 2 {
		t.Fatalf("deltas = %#v, want only the two explicit deltas", deltas)
	}
	if got := strings.Join(deltas, ""); got != "我是 Claude。具体来说，通过 Claude Agent SDK 运行。" {
		t.Fatalf("deltas joined = %q", got)
	}
	for _, delta := range deltas {
		if delta == "我是 Claude。具体来说，我是通过 Claude Agent SDK 运行。" {
			t.Fatalf("final assistant snapshot was repeated as delta: %#v", deltas)
		}
	}
}

func TestRunClaudeReportsClearErrorWhenClaudeIsNotInPath(t *testing.T) {
	workspaceRoot := t.TempDir()

	_, err := RunClaude(RunRequest{Prompt: "hello", Cwd: workspaceRoot}, RunnerOptions{
		WorkspaceRoot: workspaceRoot,
		Env:           []string{"PATH="},
		Timeout:       5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error")
	}

	var requestErr *RequestError
	if !errors.As(err, &requestErr) {
		t.Fatalf("expected RequestError, got %T", err)
	}
	if requestErr.StatusCode != 503 {
		t.Fatalf("status = %d, want 503", requestErr.StatusCode)
	}
	if requestErr.Message != "claude CLI not found in PATH" {
		t.Fatalf("message = %q", requestErr.Message)
	}
}

func TestParseClaudeOutputRejectsInvalidJSON(t *testing.T) {
	_, err := ParseClaudeOutput("not-json")
	if err == nil {
		t.Fatal("expected error")
	}

	var requestErr *RequestError
	if !errors.As(err, &requestErr) {
		t.Fatalf("expected RequestError, got %T", err)
	}
	if requestErr.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", requestErr.StatusCode)
	}
	if requestErr.Message != "claude returned invalid JSON" {
		t.Fatalf("message = %q", requestErr.Message)
	}
}

func TestParseClaudeStreamOutputReadsLastResult(t *testing.T) {
	output, err := ParseClaudeStreamOutput("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hello\"}]}}\n{\"type\":\"result\",\"result\":\"final text\"}\n")
	if err != nil {
		t.Fatal(err)
	}
	if output != "final text" {
		t.Fatalf("output = %q, want final text", output)
	}
}

func fakeClaudeStreamScript() string {
	return `#!/bin/sh
saw_stream_json=0
saw_verbose=0
saw_partial=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-format)
      shift
      if [ "$1" = "stream-json" ]; then
        saw_stream_json=1
      fi
      ;;
    --verbose)
      saw_verbose=1
      ;;
    --include-partial-messages)
      saw_partial=1
      ;;
  esac
  shift
done
if [ "$saw_stream_json$saw_verbose$saw_partial" != "111" ]; then
  printf 'missing required stream args' >&2
  exit 7
fi
prompt=$(cat)
printf '{"type":"assistant","message":{"content":[{"type":"text","text":"stream:"}]}}\n'
printf '{"type":"assistant","message":{"content":[{"type":"text","text":"stream:%s"}]}}\n' "$prompt"
printf '{"type":"result","result":"final:%s"}\n' "$prompt"
`
}

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

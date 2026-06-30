package agenthttp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type collectStreamWriter struct {
	deltas []string
}

func (w *collectStreamWriter) WriteDelta(delta string) error {
	w.deltas = append(w.deltas, delta)
	return nil
}

func TestValidatePromptRejectsMissingPrompt(t *testing.T) {
	_, err := ValidatePrompt(RunRequest{})
	if err == nil {
		t.Fatal("expected error")
	}

	var requestErr *RequestError
	if !errors.As(err, &requestErr) {
		t.Fatalf("expected RequestError, got %T", err)
	}
	if requestErr.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", requestErr.StatusCode)
	}
	if requestErr.Message != "prompt must be a non-empty string" {
		t.Fatalf("message = %q", requestErr.Message)
	}
}

func TestResolveWorkspaceCwdRejectsCwdOutsideWorkspace(t *testing.T) {
	workspaceRoot := t.TempDir()
	outside := filepath.Dir(workspaceRoot)

	_, err := ResolveWorkspaceCwd(outside, workspaceRoot)
	if err == nil {
		t.Fatal("expected error")
	}

	var requestErr *RequestError
	if !errors.As(err, &requestErr) {
		t.Fatalf("expected RequestError, got %T", err)
	}
	if requestErr.Message != "cwd must be inside workspace" {
		t.Fatalf("message = %q", requestErr.Message)
	}
}

func TestResolveWorkspaceCwdAllowsWorkspaceRootAndChildren(t *testing.T) {
	workspaceRoot := t.TempDir()
	child := filepath.Join(workspaceRoot, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}

	root, err := ResolveWorkspaceCwd("", workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if root != workspaceRoot {
		t.Fatalf("root = %q, want %q", root, workspaceRoot)
	}

	resolvedChild, err := ResolveWorkspaceCwd(child, workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedChild != child {
		t.Fatalf("child = %q, want %q", resolvedChild, child)
	}
}

func TestRunCodexExecutesFakeCodexAndReturnsOutputFileContent(t *testing.T) {
	workspaceRoot := t.TempDir()
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "codex", fakeCodexScript())

	result, err := RunCodex(RunRequest{Prompt: "hello", Cwd: workspaceRoot}, RunnerOptions{
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
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("exitCode = %v, want 0", result.ExitCode)
	}
	if result.Output != "final:hello" {
		t.Fatalf("output = %q", result.Output)
	}
	if result.Stdout != "stdout text" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	if result.Stderr != "stderr text" {
		t.Fatalf("stderr = %q", result.Stderr)
	}
}

func TestRunCodexStreamUsesAppServerDeltasAndCompletedTurnOutput(t *testing.T) {
	workspaceRoot := t.TempDir()
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "codex", fakeCodexAppServerScript())

	writer := &collectStreamWriter{}
	result, err := RunCodexStreamContext(context.Background(), RunRequest{Prompt: "hello", Cwd: workspaceRoot}, writer, RunnerOptions{
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
	if !strings.Contains(result.Stdout, `"item/agentMessage/delta"`) {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	if strings.Join(writer.deltas, "") != "stream:hello" {
		t.Fatalf("deltas = %#v, want stream:hello", writer.deltas)
	}
}

func TestRunCodexStreamSendsAbsoluteWorkspaceRoots(t *testing.T) {
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "codex", fakeCodexAppServerAbsolutePathScript())

	writer := &collectStreamWriter{}
	result, err := RunCodexStreamContext(context.Background(), RunRequest{Prompt: "hello"}, writer, RunnerOptions{
		WorkspaceRoot: ".",
		Env:           envWithPath(binDir),
		Timeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("ok = false, error = %q, stderr = %q", result.Error, result.Stderr)
	}
}

func TestRunCodexStreamRejectsUnsupportedAppServerRequest(t *testing.T) {
	workspaceRoot := t.TempDir()
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "codex", fakeCodexAppServerRequestScript())

	writer := &collectStreamWriter{}
	result, err := RunCodexStreamContext(context.Background(), RunRequest{Prompt: "hello", Cwd: workspaceRoot}, writer, RunnerOptions{
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
	if result.Output != "final" {
		t.Fatalf("output = %q, want final", result.Output)
	}
}

func TestCodexAppServerDeltaTrackerConvertsCumulativeDeltaToSuffix(t *testing.T) {
	writer := &collectStreamWriter{}
	threadID := "thread-1"
	turnID := "turn-1"
	result := codexAppServerRun{}
	tracker := newAppServerDeltaTracker()
	sendRequest := func(int, string, any) error { return nil }
	sendResponseError := func(any, int, string) error { return nil }

	lines := []string{
		`{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"抱歉，"}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"抱歉，查询天气需要发起网络请求"}}`,
	}
	for _, line := range lines {
		if err := handleCodexAppServerLine(line, writer, sendRequest, sendResponseError, "", "", "", &threadID, &turnID, tracker, &result); err != nil {
			t.Fatal(err)
		}
	}

	if got := strings.Join(writer.deltas, ""); got != "抱歉，查询天气需要发起网络请求" {
		t.Fatalf("deltas = %#v, joined = %q", writer.deltas, got)
	}
	if len(writer.deltas) != 2 {
		t.Fatalf("deltas = %#v, want original prefix plus suffix only", writer.deltas)
	}
	if writer.deltas[1] != "查询天气需要发起网络请求" {
		t.Fatalf("second delta = %q, want suffix only", writer.deltas[1])
	}
}

func TestCodexAppServerRespondsToUnsupportedServerRequest(t *testing.T) {
	threadID := "thread-1"
	turnID := "turn-1"
	result := codexAppServerRun{}
	tracker := newAppServerDeltaTracker()
	sendRequest := func(int, string, any) error { return nil }

	var (
		gotID      any
		gotCode    int
		gotMessage string
	)
	sendResponseError := func(id any, code int, message string) error {
		gotID = id
		gotCode = code
		gotMessage = message
		return nil
	}

	err := handleCodexAppServerLine(
		`{"id":99,"method":"currentTime/read","params":{}}`,
		nil,
		sendRequest,
		sendResponseError,
		"",
		"",
		"",
		&threadID,
		&turnID,
		tracker,
		&result,
	)
	if err != nil {
		t.Fatal(err)
	}

	if gotID != float64(99) {
		t.Fatalf("response id = %#v, want 99", gotID)
	}
	if gotCode != -32000 {
		t.Fatalf("error code = %d, want -32000", gotCode)
	}
	if gotMessage != "server request is not supported by agent-http-go" {
		t.Fatalf("error message = %q", gotMessage)
	}
}

func TestRunCodexReturnsNonZeroFakeCodexFailure(t *testing.T) {
	workspaceRoot := t.TempDir()
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "codex", "#!/bin/sh\nprintf 'failed badly' >&2\nexit 7\n")

	result, err := RunCodex(RunRequest{Prompt: "hello", Cwd: workspaceRoot}, RunnerOptions{
		WorkspaceRoot: workspaceRoot,
		Env:           envWithPath(binDir),
		Timeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.OK {
		t.Fatal("ok = true, want false")
	}
	if result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("exitCode = %v, want 7", result.ExitCode)
	}
	if result.Error != "codex exited with code 7" {
		t.Fatalf("error = %q", result.Error)
	}
	if result.Stderr != "failed badly" {
		t.Fatalf("stderr = %q", result.Stderr)
	}
}

func TestRunCodexReportsClearErrorWhenCodexIsNotInPath(t *testing.T) {
	workspaceRoot := t.TempDir()

	_, err := RunCodex(RunRequest{Prompt: "hello", Cwd: workspaceRoot}, RunnerOptions{
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
	if requestErr.Message != "codex CLI not found in PATH" {
		t.Fatalf("message = %q", requestErr.Message)
	}
}

func TestRunCodexTimesOutAndTerminatesFakeCodex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake command is POSIX-only")
	}

	workspaceRoot := t.TempDir()
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "codex", "#!/bin/sh\nsleep 10\n")

	result, err := RunCodex(RunRequest{Prompt: "hello", Cwd: workspaceRoot}, RunnerOptions{
		WorkspaceRoot: workspaceRoot,
		Env:           envWithPath(binDir),
		Timeout:       50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.OK {
		t.Fatal("ok = true, want false")
	}
	if result.Error != "codex execution timed out" {
		t.Fatalf("error = %q", result.Error)
	}
	if !result.TimedOut {
		t.Fatal("timedOut = false, want true")
	}
}

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
		`{"type":"assistant","message":{"content":[{"type":"text","text":"请随时告知"}],"stop_reason":"end_turn"}}`,
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

func TestClaudeJSONLDeltaParserIgnoresFinalAssistantSnapshot(t *testing.T) {
	parser := newClaudeJSONLDeltaParser()

	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"代码"}],"stop_reason":null}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"代码：处理文件"}],"stop_reason":null}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"我无法查询实时天气数据，因为我没有联网搜索的能力。"}],"stop_reason":"end_turn"}}`,
	}

	var deltas []string
	for _, line := range lines {
		deltas = append(deltas, parser.Deltas(line)...)
	}

	if got := strings.Join(deltas, ""); got != "代码：处理文件" {
		t.Fatalf("deltas = %#v, joined = %q, want only partial assistant text", deltas, got)
	}
	if len(deltas) != 2 {
		t.Fatalf("deltas = %#v, want final assistant snapshot ignored", deltas)
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

func writeFakeCommand(t *testing.T, binDir, command, source string) {
	t.Helper()

	path := filepath.Join(binDir, command)
	if err := os.WriteFile(path, []byte(source), 0o755); err != nil {
		t.Fatal(err)
	}
}

func envWithPath(binDir string) []string {
	env := os.Environ()
	for i, item := range env {
		if strings.HasPrefix(item, "PATH=") {
			env[i] = "PATH=" + binDir + string(os.PathListSeparator) + strings.TrimPrefix(item, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+binDir)
}

func fakeCodexScript() string {
	return `#!/bin/sh
output=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    shift
    output="$1"
  fi
  shift
done
prompt=$(cat)
printf 'final:%s' "$prompt" > "$output"
printf 'stdout text'
printf 'stderr text' >&2
`
}

func fakeCodexAppServerScript() string {
	return `#!/bin/sh
if [ "$1" != "app-server" ] || [ "$2" != "--stdio" ]; then
  printf 'unexpected args: %s %s' "$1" "$2" >&2
  exit 7
fi
IFS= read -r initialize
printf '{"id":1,"result":{"userAgent":"fake","codexHome":"/tmp","platformFamily":"unix","platformOs":"linux"}}\n'
IFS= read -r thread_start
printf '{"id":2,"result":{"thread":{"id":"thread-1"}}}\n'
IFS= read -r turn_start
case "$turn_start" in
  *'"text":"hello"'*) ;;
  *)
    printf 'turn/start did not include prompt: %s' "$turn_start" >&2
    exit 8
    ;;
esac
printf '{"id":3,"result":{"turn":{"id":"turn-1"}}}\n'
printf '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"stream:"}}\n'
printf '{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","delta":"hello"}}\n'
printf '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[{"type":"agentMessage","id":"item-1","text":"final:hello"}],"itemsView":"full","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1}}}\n'
printf 'stderr text' >&2
`
}

func fakeCodexAppServerAbsolutePathScript() string {
	return `#!/bin/sh
IFS= read -r initialize
printf '{"id":1,"result":{"userAgent":"fake","codexHome":"/tmp","platformFamily":"unix","platformOs":"linux"}}\n'
IFS= read -r thread_start
if ! printf '%s' "$thread_start" | grep -q '"runtimeWorkspaceRoots":\["/'; then
  printf 'thread/start did not include absolute runtimeWorkspaceRoots: %s' "$thread_start" >&2
  exit 8
fi
printf '{"id":2,"result":{"thread":{"id":"thread-1"}}}\n'
IFS= read -r turn_start
if ! printf '%s' "$turn_start" | grep -q '"runtimeWorkspaceRoots":\["/'; then
  printf 'turn/start did not include absolute runtimeWorkspaceRoots: %s' "$turn_start" >&2
  exit 9
fi
printf '{"id":3,"result":{"turn":{"id":"turn-1"}}}\n'
printf '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[{"type":"agentMessage","id":"item-1","text":"final"}],"itemsView":"full","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1}}}\n'
`
}

func fakeCodexAppServerRequestScript() string {
	return `#!/bin/sh
IFS= read -r initialize
printf '{"id":1,"result":{"userAgent":"fake","codexHome":"/tmp","platformFamily":"unix","platformOs":"linux"}}\n'
IFS= read -r thread_start
printf '{"id":2,"result":{"thread":{"id":"thread-1"}}}\n'
IFS= read -r turn_start
printf '{"id":3,"result":{"turn":{"id":"turn-1"}}}\n'
printf '{"id":99,"method":"currentTime/read","params":{}}\n'
IFS= read -r unsupported_response
case "$unsupported_response" in
  *'"id":99'*'"error"'*'server request is not supported by agent-http-go'*) ;;
  *)
    printf 'unsupported request response was not sent: %s' "$unsupported_response" >&2
    exit 8
    ;;
esac
printf '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[{"type":"agentMessage","id":"item-1","text":"final"}],"itemsView":"full","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1}}}\n'
`
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
printf '{"type":"content_block_delta","delta":{"type":"text_delta","text":"stream:"}}\n'
printf '{"type":"content_block_delta","delta":{"type":"text_delta","text":"%s"}}\n' "$prompt"
printf '{"type":"assistant","message":{"content":[{"type":"text","text":"stream:%s"}],"stop_reason":"end_turn"}}\n' "$prompt"
printf '{"type":"result","result":"final:%s"}\n' "$prompt"
`
}

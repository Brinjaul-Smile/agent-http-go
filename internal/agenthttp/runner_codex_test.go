package agenthttp

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

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

func TestRunCodexStreamUsesConfiguredAppServerOptions(t *testing.T) {
	workspaceRoot := t.TempDir()
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "custom-codex", fakeCodexAppServerOptionsScript())

	ephemeral := false
	writer := &collectStreamWriter{}
	result, err := RunCodexStreamContext(context.Background(), RunRequest{Prompt: "hello", Cwd: workspaceRoot}, writer, RunnerOptions{
		WorkspaceRoot: workspaceRoot,
		Env:           envWithPath(binDir),
		Timeout:       5 * time.Second,
		CodexCommand:  "custom-codex",
		CodexAppServerOptions: CodexAppServerOptions{
			ApprovalPolicy: "on-request",
			Sandbox:        "read-only",
			Ephemeral:      &ephemeral,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !result.OK {
		t.Fatalf("ok = false, error = %q, stderr = %q", result.Error, result.Stderr)
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

func fakeCodexAppServerOptionsScript() string {
	return `#!/bin/sh
IFS= read -r initialize
printf '{"id":1,"result":{"userAgent":"fake","codexHome":"/tmp","platformFamily":"unix","platformOs":"linux"}}\n'
IFS= read -r thread_start
if ! printf '%s' "$thread_start" | grep -q '"approvalPolicy":"on-request"'; then
  printf 'thread/start did not include configured approvalPolicy: %s' "$thread_start" >&2
  exit 8
fi
if ! printf '%s' "$thread_start" | grep -q '"sandbox":"read-only"'; then
  printf 'thread/start did not include configured sandbox: %s' "$thread_start" >&2
  exit 9
fi
if ! printf '%s' "$thread_start" | grep -q '"ephemeral":false'; then
  printf 'thread/start did not include configured ephemeral: %s' "$thread_start" >&2
  exit 10
fi
printf '{"id":2,"result":{"thread":{"id":"thread-1"}}}\n'
IFS= read -r turn_start
printf '{"id":3,"result":{"turn":{"id":"turn-1"}}}\n'
printf '{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","items":[{"type":"agentMessage","id":"item-1","text":"final"}],"itemsView":"full","status":"completed","error":null,"startedAt":1,"completedAt":2,"durationMs":1}}}\n'
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

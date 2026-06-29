package agenthttp

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

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

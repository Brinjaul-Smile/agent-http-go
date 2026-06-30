package agenthttp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

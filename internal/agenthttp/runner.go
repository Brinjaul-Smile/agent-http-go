package agenthttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const DefaultTimeout = 10 * time.Minute

type RunRequest struct {
	Agent  string `json:"agent"`
	Prompt string `json:"prompt"`
	Cwd    string `json:"cwd"`
}

type RunResult struct {
	OK       bool
	Error    string
	ExitCode *int
	Signal   string
	Output   string
	TimedOut bool
	Stdout   string
	Stderr   string
}

type Runner func(RunRequest) (RunResult, error)

type RunnerOptions struct {
	WorkspaceRoot string
	Env           []string
	Timeout       time.Duration
}

func ValidatePrompt(body RunRequest) (string, error) {
	if strings.TrimSpace(body.Prompt) == "" {
		return "", NewRequestError("prompt must be a non-empty string", http.StatusBadRequest)
	}
	return body.Prompt, nil
}

func ResolveWorkspaceCwd(inputCwd, workspaceRoot string) (string, error) {
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", err
	}

	requested := root
	if inputCwd != "" {
		requested, err = filepath.Abs(inputCwd)
		if err != nil {
			return "", err
		}
	}

	relative, err := filepath.Rel(root, requested)
	if err != nil {
		return "", err
	}
	if relative == "." || (!strings.HasPrefix(relative, "..") && !filepath.IsAbs(relative)) {
		return requested, nil
	}

	return "", NewRequestError("cwd must be inside workspace", http.StatusBadRequest)
}

func RunCodex(body RunRequest, options RunnerOptions) (RunResult, error) {
	env := runnerEnv(options.Env)
	workspaceRoot := runnerWorkspaceRoot(options.WorkspaceRoot)
	timeout := runnerTimeout(options.Timeout)

	prompt, err := ValidatePrompt(body)
	if err != nil {
		return RunResult{}, err
	}

	cwd, err := ResolveWorkspaceCwd(body.Cwd, workspaceRoot)
	if err != nil {
		return RunResult{}, err
	}

	path, err := FindExecutable("codex", env)
	if err != nil {
		return RunResult{}, err
	}
	if path == "" {
		return RunResult{}, NewRequestError("codex CLI not found in PATH", http.StatusServiceUnavailable)
	}

	tempDir, err := os.MkdirTemp("", "codex-http-")
	if err != nil {
		return RunResult{}, err
	}
	defer os.RemoveAll(tempDir)

	outputPath := filepath.Join(tempDir, "last-message.txt")
	childResult := runChild(prompt, timeout, execCommandSpec{
		Name: path,
		Args: []string{"exec", "--skip-git-repo-check", "-C", cwd, "-o", outputPath, "-"},
		Cwd:  cwd,
		Env:  env,
	})

	output, readErr := readOutputFile(outputPath)
	if readErr != nil {
		return RunResult{}, readErr
	}

	if childResult.TimedOut {
		return RunResult{
			OK:       false,
			Error:    "codex execution timed out",
			ExitCode: childResult.ExitCode,
			Signal:   childResult.Signal,
			TimedOut: true,
			Output:   output,
			Stdout:   childResult.Stdout,
			Stderr:   childResult.Stderr,
		}, nil
	}
	if childResult.Err != nil {
		return RunResult{}, childResult.Err
	}
	if childResult.ExitCode == nil || *childResult.ExitCode != 0 {
		code := 0
		if childResult.ExitCode != nil {
			code = *childResult.ExitCode
		}
		return RunResult{
			OK:       false,
			Error:    "codex exited with code " + intString(code),
			ExitCode: childResult.ExitCode,
			Signal:   childResult.Signal,
			Output:   output,
			Stdout:   childResult.Stdout,
			Stderr:   childResult.Stderr,
		}, nil
	}

	return RunResult{
		OK:       true,
		ExitCode: childResult.ExitCode,
		Output:   output,
		Stdout:   childResult.Stdout,
		Stderr:   childResult.Stderr,
	}, nil
}

func RunClaude(body RunRequest, options RunnerOptions) (RunResult, error) {
	env := runnerEnv(options.Env)
	workspaceRoot := runnerWorkspaceRoot(options.WorkspaceRoot)
	timeout := runnerTimeout(options.Timeout)

	prompt, err := ValidatePrompt(body)
	if err != nil {
		return RunResult{}, err
	}

	cwd, err := ResolveWorkspaceCwd(body.Cwd, workspaceRoot)
	if err != nil {
		return RunResult{}, err
	}

	path, err := FindExecutable("claude", env)
	if err != nil {
		return RunResult{}, err
	}
	if path == "" {
		return RunResult{}, NewRequestError("claude CLI not found in PATH", http.StatusServiceUnavailable)
	}

	childResult := runChild(prompt, timeout, execCommandSpec{
		Name: path,
		Args: []string{"--bare", "-p", "--output-format", "json"},
		Cwd:  cwd,
		Env:  env,
	})

	if childResult.TimedOut {
		return RunResult{
			OK:       false,
			Error:    "claude execution timed out",
			ExitCode: childResult.ExitCode,
			Signal:   childResult.Signal,
			TimedOut: true,
			Output:   "",
			Stdout:   childResult.Stdout,
			Stderr:   childResult.Stderr,
		}, nil
	}
	if childResult.Err != nil {
		return RunResult{}, childResult.Err
	}
	if childResult.ExitCode == nil || *childResult.ExitCode != 0 {
		code := 0
		if childResult.ExitCode != nil {
			code = *childResult.ExitCode
		}
		return RunResult{
			OK:       false,
			Error:    "claude exited with code " + intString(code),
			ExitCode: childResult.ExitCode,
			Signal:   childResult.Signal,
			Output:   "",
			Stdout:   childResult.Stdout,
			Stderr:   childResult.Stderr,
		}, nil
	}

	output, err := ParseClaudeOutput(childResult.Stdout)
	if err != nil {
		return RunResult{}, err
	}

	return RunResult{
		OK:       true,
		ExitCode: childResult.ExitCode,
		Output:   output,
		Stdout:   childResult.Stdout,
		Stderr:   childResult.Stderr,
	}, nil
}

func ParseClaudeOutput(stdout string) (string, error) {
	var payload struct {
		Result any `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		return "", NewRequestError("claude returned invalid JSON", http.StatusBadGateway)
	}
	if result, ok := payload.Result.(string); ok {
		return result, nil
	}
	return stdout, nil
}

type execCommandSpec struct {
	Name string
	Args []string
	Cwd  string
	Env  []string
}

type childResult struct {
	ExitCode *int
	Signal   string
	Stdout   string
	Stderr   string
	TimedOut bool
	Err      error
}

func runChild(prompt string, timeout time.Duration, spec execCommandSpec) childResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = spec.Env
	cmd.Stdin = strings.NewReader(prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := childResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if ctx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		result.ExitCode = &code
		result.Signal = signalName(exitErr)
		return result
	}
	if err != nil {
		result.Err = err
		return result
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.ExitCode = &code
	}

	return result
}

func readOutputFile(outputPath string) (string, error) {
	payload, err := os.ReadFile(outputPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(payload), nil
}

func runnerEnv(env []string) []string {
	if env != nil {
		return env
	}
	return os.Environ()
}

func runnerWorkspaceRoot(workspaceRoot string) string {
	if workspaceRoot != "" {
		return workspaceRoot
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

func runnerTimeout(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return DefaultTimeout
}

func intString(value int) string {
	return strconv.Itoa(value)
}

func signalName(exitErr *exec.ExitError) string {
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	return status.Signal().String()
}

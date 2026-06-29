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

// DefaultTimeout 是单次 agent CLI 执行允许的最长时间。
const DefaultTimeout = 10 * time.Minute

// RunRequest 表示 /runs 和 /codex 接口接收的 JSON 请求体。
type RunRequest struct {
	Agent  string `json:"agent"`
	Prompt string `json:"prompt"`
	Cwd    string `json:"cwd"`
}

// RunResult 是不同 agent runner 统一返回给 HTTP 层的执行结果。
type RunResult struct {
	OK       bool
	Error    string
	ExitCode *int
	Output   string
	TimedOut bool
	Stdout   string
	Stderr   string
}

// Runner 执行一次 agent 请求。
type Runner func(RunRequest) (RunResult, error)

// RunnerOptions 注入进程执行所需的工作区、环境变量和超时时间。
type RunnerOptions struct {
	WorkspaceRoot string
	Env           []string
	Timeout       time.Duration
}

// ValidatePrompt 校验所有 runner 共享的 prompt 非空规则。
func ValidatePrompt(body RunRequest) (string, error) {
	if strings.TrimSpace(body.Prompt) == "" {
		return "", NewRequestError("prompt must be a non-empty string", http.StatusBadRequest)
	}
	return body.Prompt, nil
}

// ResolveWorkspaceCwd 解析请求传入的 cwd，并防止它逃逸出服务工作区。
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

// RunCodex 以非交互方式执行 codex，并从 codex 写出的输出文件读取最终回复。
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

// RunClaude 以 JSON 输出模式执行 Claude Code，并把 result 字段归一化成 RunResult。
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

// ParseClaudeOutput 从 Claude 的 JSON stdout 中提取 result 字符串。
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

// childResult 保存子进程退出后的原始执行信息。
type childResult struct {
	ExitCode *int
	Stdout   string
	Stderr   string
	TimedOut bool
	Err      error
}

// runChild 启动 CLI 子进程，把 prompt 写入 stdin，并收集 stdout/stderr。
func runChild(prompt string, timeout time.Duration, spec execCommandSpec) childResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = spec.Env
	cmd.Stdin = strings.NewReader(prompt)

	// 将 CLI 放到独立进程组里，超时时可以一起终止 shell wrapper 和子进程，
	// 避免只杀掉 exec 启动的第一个进程。
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

// readOutputFile 读取 codex -o 参数写出的最终消息文件。
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

// runnerEnv 返回 runner 使用的环境变量，未显式传入时使用当前进程环境。
func runnerEnv(env []string) []string {
	if env != nil {
		return env
	}
	return os.Environ()
}

// runnerWorkspaceRoot 返回 runner 的工作区根目录，未显式传入时使用当前目录。
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

// runnerTimeout 返回 runner 超时时间，未显式传入时使用默认值。
func runnerTimeout(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return DefaultTimeout
}

// intString 将退出码转换成响应错误文案需要的字符串。
func intString(value int) string {
	return strconv.Itoa(value)
}

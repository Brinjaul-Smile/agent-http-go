package agenthttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	// SessionID 只由持久化会话接口填充；普通 /runs 和 /codex 保持空值。
	SessionID string
}

// Runner 执行一次 agent 请求。
type Runner func(context.Context, RunRequest) (RunResult, error)

// StreamWriter 接收 runner 实时输出的文本片段。
type StreamWriter interface {
	WriteDelta(string) error
}

// StreamRunner 执行一次 agent 请求，并尽量把 CLI stdout 的增量片段写给调用方。
type StreamRunner func(context.Context, RunRequest, StreamWriter) (RunResult, error)

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
	return RunCodexContext(context.Background(), body, options)
}

// RunCodexContext 以非交互方式执行 codex，并从 codex 写出的输出文件读取最终回复。
func RunCodexContext(ctx context.Context, body RunRequest, options RunnerOptions) (RunResult, error) {
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
	childResult := runChild(ctx, prompt, timeout, execCommandSpec{
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

// RunCodexStreamContext 以 SSE 兼容方式执行 codex。
// codex exec --json 当前不提供 Claude 风格的正文增量输出；流式接口改用
// experimental app-server 协议里的 item/agentMessage/delta 通知。
func RunCodexStreamContext(ctx context.Context, body RunRequest, writer StreamWriter, options RunnerOptions) (RunResult, error) {
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
	absoluteWorkspaceRoot, err := filepath.Abs(workspaceRoot)
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

	appResult := runCodexAppServerStream(ctx, prompt, timeout, execCommandSpec{
		Name: path,
		Args: []string{"app-server", "--stdio"},
		Cwd:  cwd,
		Env:  env,
	}, writer, cwd, absoluteWorkspaceRoot)

	return codexAppServerRunResult(appResult)
}

// RunClaude 以 JSON 输出模式执行 Claude Code，并把 result 字段归一化成 RunResult。
func RunClaude(body RunRequest, options RunnerOptions) (RunResult, error) {
	return RunClaudeContext(context.Background(), body, options)
}

// RunClaudeContext 以 JSON 输出模式执行 Claude Code，并把 result 字段归一化成 RunResult。
func RunClaudeContext(ctx context.Context, body RunRequest, options RunnerOptions) (RunResult, error) {
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

	childResult := runChild(ctx, prompt, timeout, execCommandSpec{
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

// RunClaudeStreamContext 以流式方式执行 Claude Code。
// 当前仍使用 Claude 的 JSON 输出模式；如果 CLI 实时写 stdout，则片段会被推给 StreamWriter。
func RunClaudeStreamContext(ctx context.Context, body RunRequest, writer StreamWriter, options RunnerOptions) (RunResult, error) {
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

	childResult := runChildStream(ctx, prompt, timeout, execCommandSpec{
		Name: path,
		Args: []string{"--bare", "-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"},
		Cwd:  cwd,
		Env:  env,
	}, newJSONLineStreamWriter(writer, newClaudeJSONLDeltaParser()))

	return claudeRunResult(childResult)
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

// ParseClaudeStreamOutput 从 Claude stream-json stdout 中提取最终 result 字段。
func ParseClaudeStreamOutput(stdout string) (string, error) {
	lines := strings.Split(stdout, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			continue
		}
		if result, ok := payload["result"].(string); ok {
			return result, nil
		}
	}
	return ParseClaudeOutput(stdout)
}

func codexRunResult(childResult childResult, output string) (RunResult, error) {
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

type codexAppServerRun struct {
	childResult
	Output      string
	TurnStatus  string
	TurnError   string
	Initialized bool
	Completed   bool
}

func codexAppServerRunResult(appResult codexAppServerRun) (RunResult, error) {
	if appResult.TimedOut {
		return RunResult{
			OK:       false,
			Error:    "codex execution timed out",
			ExitCode: appResult.ExitCode,
			TimedOut: true,
			Output:   appResult.Output,
			Stdout:   appResult.Stdout,
			Stderr:   appResult.Stderr,
		}, nil
	}
	if appResult.Err != nil {
		return RunResult{}, appResult.Err
	}
	if appResult.ExitCode == nil || *appResult.ExitCode != 0 {
		code := 0
		if appResult.ExitCode != nil {
			code = *appResult.ExitCode
		}
		return RunResult{
			OK:       false,
			Error:    "codex exited with code " + intString(code),
			ExitCode: appResult.ExitCode,
			Output:   appResult.Output,
			Stdout:   appResult.Stdout,
			Stderr:   appResult.Stderr,
		}, nil
	}
	if !appResult.Completed {
		return RunResult{
			OK:       false,
			Error:    "codex app-server ended before turn completed",
			ExitCode: appResult.ExitCode,
			Output:   appResult.Output,
			Stdout:   appResult.Stdout,
			Stderr:   appResult.Stderr,
		}, nil
	}
	if appResult.TurnStatus != "" && appResult.TurnStatus != "completed" {
		message := appResult.TurnError
		if message == "" {
			message = "codex turn " + appResult.TurnStatus
		}
		return RunResult{
			OK:       false,
			Error:    message,
			ExitCode: appResult.ExitCode,
			Output:   appResult.Output,
			Stdout:   appResult.Stdout,
			Stderr:   appResult.Stderr,
		}, nil
	}

	return RunResult{
		OK:       true,
		ExitCode: appResult.ExitCode,
		Output:   appResult.Output,
		Stdout:   appResult.Stdout,
		Stderr:   appResult.Stderr,
	}, nil
}

func claudeRunResult(childResult childResult) (RunResult, error) {
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

	output, err := ParseClaudeStreamOutput(childResult.Stdout)
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
func runChild(ctx context.Context, prompt string, timeout time.Duration, spec execCommandSpec) childResult {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(spec.Name, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = spec.Env
	cmd.Stdin = strings.NewReader(prompt)

	// 将 CLI 放到独立进程组里，取消时可以一起终止 shell wrapper 和子进程，
	// 避免只杀掉 exec 启动的第一个进程。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	result := childResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err := ctx.Err(); err != nil {
		result.Err = err
		return result
	}

	if err := cmd.Start(); err != nil {
		result.Err = err
		return result
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()

	var err error
	select {
	case err = <-errCh:
	case <-ctx.Done():
		err = stopProcessGroup(cmd, errCh)
		if ctx.Err() == context.DeadlineExceeded {
			result.TimedOut = true
		} else {
			result.Err = ctx.Err()
		}
	}

	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

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

// runChildStream 启动 CLI 子进程，并把 stdout 读取到的片段实时写给 StreamWriter。
func runChildStream(ctx context.Context, prompt string, timeout time.Duration, spec execCommandSpec, writer StreamWriter) childResult {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(spec.Name, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = spec.Env
	cmd.Stdin = strings.NewReader(prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return childResult{Err: err}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return childResult{Err: err}
	}

	if err := ctx.Err(); err != nil {
		return childResult{Err: err}
	}
	if err := cmd.Start(); err != nil {
		return childResult{Err: err}
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	stdoutDone := make(chan error, 1)
	stderrDone := make(chan error, 1)

	go func() {
		stdoutDone <- copyStreamOutput(stdoutPipe, &stdout, writer)
	}()
	go func() {
		_, err := io.Copy(&stderr, stderrPipe)
		stderrDone <- err
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()

	var waitErr error
	result := childResult{}
	select {
	case waitErr = <-errCh:
	case <-ctx.Done():
		waitErr = stopProcessGroup(cmd, errCh)
		if ctx.Err() == context.DeadlineExceeded {
			result.TimedOut = true
		} else {
			result.Err = ctx.Err()
		}
	}

	if err := <-stdoutDone; err != nil && result.Err == nil {
		result.Err = err
	}
	if err := <-stderrDone; err != nil && !isClosedPipeReadError(err) && result.Err == nil {
		result.Err = err
	}

	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code := exitErr.ExitCode()
		result.ExitCode = &code
		return result
	}
	if waitErr != nil && result.Err == nil {
		result.Err = waitErr
		return result
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.ExitCode = &code
	}

	return result
}

type appServerLine struct {
	line string
	err  error
}

type appServerMessage struct {
	ID     any             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *appServerError `json:"error,omitempty"`
}

type appServerError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type appServerRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type appServerResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *appServerError `json:"error,omitempty"`
}

type appServerThreadStartResult struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
}

type appServerTurnStartResult struct {
	Turn struct {
		ID string `json:"id"`
	} `json:"turn"`
}

type appServerDeltaParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type appServerTurnCompletedParams struct {
	ThreadID string        `json:"threadId"`
	Turn     appServerTurn `json:"turn"`
}

type appServerTurn struct {
	ID     string              `json:"id"`
	Status string              `json:"status"`
	Error  *appServerTurnError `json:"error"`
	Items  []appServerItem     `json:"items"`
}

type appServerTurnError struct {
	Message string `json:"message"`
}

type appServerItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func runCodexAppServerStream(ctx context.Context, prompt string, timeout time.Duration, spec execCommandSpec, writer StreamWriter, cwd string, workspaceRoot string) codexAppServerRun {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(spec.Name, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = spec.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return codexAppServerRun{childResult: childResult{Err: err}}
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return codexAppServerRun{childResult: childResult{Err: err}}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return codexAppServerRun{childResult: childResult{Err: err}}
	}

	if err := ctx.Err(); err != nil {
		return codexAppServerRun{childResult: childResult{Err: err}}
	}
	if err := cmd.Start(); err != nil {
		return codexAppServerRun{childResult: childResult{Err: err}}
	}

	var (
		stdoutMu sync.Mutex
		stdout   bytes.Buffer
		stderr   bytes.Buffer
	)
	lineCh := make(chan appServerLine, 1)
	stderrDone := make(chan error, 1)
	waitCh := make(chan error, 1)

	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			stdoutMu.Lock()
			stdout.WriteString(line)
			stdout.WriteByte('\n')
			stdoutMu.Unlock()
			lineCh <- appServerLine{line: line}
		}
		if err := scanner.Err(); err != nil && !isClosedPipeReadError(err) {
			lineCh <- appServerLine{err: err}
		}
		close(lineCh)
	}()
	go func() {
		_, err := io.Copy(&stderr, stderrPipe)
		stderrDone <- err
	}()
	go func() {
		waitCh <- cmd.Wait()
	}()

	result := codexAppServerRun{}
	sendRequest := func(id int, method string, params any) error {
		payload, err := json.Marshal(appServerRequest{
			JSONRPC: "2.0",
			ID:      id,
			Method:  method,
			Params:  params,
		})
		if err != nil {
			return err
		}
		if _, err := stdinPipe.Write(append(payload, '\n')); err != nil {
			return err
		}
		return nil
	}
	sendResponseError := func(id any, code int, message string) error {
		payload, err := json.Marshal(appServerResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &appServerError{
				Code:    code,
				Message: message,
			},
		})
		if err != nil {
			return err
		}
		if _, err := stdinPipe.Write(append(payload, '\n')); err != nil {
			return err
		}
		return nil
	}

	if err := sendRequest(1, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "agent-http-go",
			"title":   nil,
			"version": "0",
		},
		"capabilities": map[string]any{
			"experimentalApi":    true,
			"requestAttestation": false,
		},
	}); err != nil {
		result.Err = err
		_ = stopProcessGroup(cmd, waitCh)
		return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, nil)
	}

	var (
		threadID     string
		turnID       string
		deltaTracker = newAppServerDeltaTracker()
	)

	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, nil)
			}
			if line.err != nil {
				result.Err = line.err
				_ = stopProcessGroup(cmd, waitCh)
				return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, nil)
			}
			if err := handleCodexAppServerLine(line.line, writer, sendRequest, sendResponseError, prompt, cwd, workspaceRoot, &threadID, &turnID, deltaTracker, &result); err != nil {
				result.Err = err
				_ = stopProcessGroup(cmd, waitCh)
				return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, nil)
			}
			if result.Completed {
				_ = stdinPipe.Close()
			}
		case err := <-waitCh:
			for line := range lineCh {
				if line.err != nil {
					if result.Err == nil {
						result.Err = line.err
					}
					continue
				}
				if handleErr := handleCodexAppServerLine(line.line, writer, sendRequest, sendResponseError, prompt, cwd, workspaceRoot, &threadID, &turnID, deltaTracker, &result); handleErr != nil && result.Err == nil {
					result.Err = handleErr
				}
			}
			return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, err)
		case <-ctx.Done():
			waitErr := stopProcessGroup(cmd, waitCh)
			if ctx.Err() == context.DeadlineExceeded {
				result.TimedOut = true
			} else {
				result.Err = ctx.Err()
			}
			return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, waitErr)
		}
	}
}

func handleCodexAppServerLine(line string, writer StreamWriter, sendRequest func(int, string, any) error, sendResponseError func(any, int, string) error, prompt string, cwd string, workspaceRoot string, threadID *string, turnID *string, deltaTracker *appServerDeltaTracker, result *codexAppServerRun) error {
	var message appServerMessage
	if err := json.Unmarshal([]byte(line), &message); err != nil {
		return nil
	}
	if message.Error != nil {
		if message.Error.Message != "" {
			return NewRequestError("codex app-server error: "+message.Error.Message, http.StatusBadGateway)
		}
		return NewRequestError("codex app-server returned an error", http.StatusBadGateway)
	}

	if id, ok := jsonNumberID(message.ID); ok {
		switch id {
		case 1:
			result.Initialized = true
			return sendRequest(2, "thread/start", map[string]any{
				"cwd":                   cwd,
				"runtimeWorkspaceRoots": []string{workspaceRoot},
				"approvalPolicy":        "never",
				"sandbox":               "workspace-write",
				"ephemeral":             true,
			})
		case 2:
			var payload appServerThreadStartResult
			if err := json.Unmarshal(message.Result, &payload); err != nil {
				return err
			}
			*threadID = payload.Thread.ID
			return sendRequest(3, "turn/start", map[string]any{
				"threadId": *threadID,
				"input": []map[string]any{{
					"type":          "text",
					"text":          prompt,
					"text_elements": []any{},
				}},
				"cwd":                   cwd,
				"runtimeWorkspaceRoots": []string{workspaceRoot},
			})
		case 3:
			var payload appServerTurnStartResult
			if err := json.Unmarshal(message.Result, &payload); err == nil {
				*turnID = payload.Turn.ID
			}
			return nil
		}
		if message.Method != "" {
			return sendResponseError(message.ID, -32000, "server request is not supported by agent-http-go")
		}
	}

	switch message.Method {
	case "item/agentMessage/delta":
		var payload appServerDeltaParams
		if err := json.Unmarshal(message.Params, &payload); err != nil {
			return nil
		}
		if payload.ThreadID != *threadID {
			return nil
		}
		if *turnID != "" && payload.TurnID != *turnID {
			return nil
		}
		if payload.Delta == "" || writer == nil {
			return nil
		}
		delta := deltaTracker.Normalize(payload)
		if delta == "" {
			return nil
		}
		return writer.WriteDelta(delta)
	case "turn/completed":
		var payload appServerTurnCompletedParams
		if err := json.Unmarshal(message.Params, &payload); err != nil {
			return nil
		}
		if payload.ThreadID != *threadID {
			return nil
		}
		if *turnID != "" && payload.Turn.ID != "" && payload.Turn.ID != *turnID {
			return nil
		}
		result.Completed = true
		result.TurnStatus = payload.Turn.Status
		if payload.Turn.Error != nil {
			result.TurnError = payload.Turn.Error.Message
		}
		result.Output = lastAgentMessageText(payload.Turn.Items)
	}

	return nil
}

type appServerDeltaTracker struct {
	seen map[string]string
}

func newAppServerDeltaTracker() *appServerDeltaTracker {
	return &appServerDeltaTracker{seen: map[string]string{}}
}

func (t *appServerDeltaTracker) Normalize(payload appServerDeltaParams) string {
	key := payload.ItemID
	if key == "" {
		key = payload.TurnID
	}
	if key == "" {
		key = payload.ThreadID
	}

	delta, next := normalizeTextDelta(t.seen[key], payload.Delta)
	t.seen[key] = next
	return delta
}

func finishCodexAppServerRun(result codexAppServerRun, cmd *exec.Cmd, waitCh <-chan error, stderrDone <-chan error, stdoutMu *sync.Mutex, stdout *bytes.Buffer, stderr *bytes.Buffer, waitErr error) codexAppServerRun {
	if waitErr == nil && cmd.ProcessState == nil {
		waitErr = <-waitCh
	}
	if err := <-stderrDone; err != nil && !isClosedPipeReadError(err) && result.Err == nil {
		result.Err = err
	}

	stdoutMu.Lock()
	result.Stdout = stdout.String()
	stdoutMu.Unlock()
	result.Stderr = stderr.String()

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code := exitErr.ExitCode()
		result.ExitCode = &code
		return result
	}
	if waitErr != nil && result.Err == nil {
		result.Err = waitErr
		return result
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.ExitCode = &code
	}
	return result
}

func jsonNumberID(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case string:
		id, err := strconv.Atoi(typed)
		return id, err == nil
	default:
		return 0, false
	}
}

func lastAgentMessageText(items []appServerItem) string {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == "agentMessage" {
			return items[i].Text
		}
	}
	return ""
}

type jsonLineStreamWriter struct {
	downstream StreamWriter
	parser     jsonLineDeltaParser
	buffer     strings.Builder
}

type jsonLineDeltaParser interface {
	Deltas(string) []string
}

func newJSONLineStreamWriter(downstream StreamWriter, parser jsonLineDeltaParser) *jsonLineStreamWriter {
	return &jsonLineStreamWriter{
		downstream: downstream,
		parser:     parser,
	}
}

func (w *jsonLineStreamWriter) WriteDelta(chunk string) error {
	w.buffer.WriteString(chunk)
	for {
		pending := w.buffer.String()
		index := strings.IndexByte(pending, '\n')
		if index < 0 {
			return nil
		}

		line := pending[:index]
		w.buffer.Reset()
		w.buffer.WriteString(pending[index+1:])
		if err := w.writeLine(line); err != nil {
			return err
		}
	}
}

func (w *jsonLineStreamWriter) Flush() error {
	line := w.buffer.String()
	w.buffer.Reset()
	return w.writeLine(line)
}

func (w *jsonLineStreamWriter) writeLine(line string) error {
	if w.downstream == nil || w.parser == nil {
		return nil
	}
	for _, delta := range w.parser.Deltas(strings.TrimSpace(line)) {
		if delta == "" {
			continue
		}
		if err := w.downstream.WriteDelta(delta); err != nil {
			return err
		}
	}
	return nil
}

type claudeJSONLDeltaParser struct {
	lastAssistantText string
}

func newClaudeJSONLDeltaParser() *claudeJSONLDeltaParser {
	return &claudeJSONLDeltaParser{}
}

func (p *claudeJSONLDeltaParser) Deltas(line string) []string {
	if line == "" {
		return nil
	}
	var payload any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		return nil
	}

	if deltas := extractExplicitDeltas(payload); len(deltas) > 0 {
		normalized := make([]string, 0, len(deltas))
		for _, delta := range deltas {
			nextDelta, nextText := normalizeTextDelta(p.lastAssistantText, delta)
			p.lastAssistantText = nextText
			if nextDelta != "" {
				normalized = append(normalized, nextDelta)
			}
		}
		return normalized
	}

	root, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	if eventType, _ := root["type"].(string); eventType != "assistant" {
		return nil
	}

	text := collectClaudeAssistantText(root["message"])
	if text == "" || text == p.lastAssistantText {
		return nil
	}
	if strings.HasPrefix(text, p.lastAssistantText) {
		delta := strings.TrimPrefix(text, p.lastAssistantText)
		p.lastAssistantText = text
		return []string{delta}
	}
	p.lastAssistantText = text
	return []string{text}
}

func normalizeTextDelta(previous string, incoming string) (string, string) {
	if incoming == "" {
		return "", previous
	}
	if previous == "" {
		return incoming, incoming
	}
	if incoming == previous || strings.HasPrefix(previous, incoming) {
		return "", previous
	}
	if strings.HasPrefix(incoming, previous) {
		return strings.TrimPrefix(incoming, previous), incoming
	}
	return incoming, previous + incoming
}

func extractExplicitDeltas(value any) []string {
	var deltas []string
	walkJSON(value, func(key string, current any) {
		if key != "delta" {
			return
		}
		switch typed := current.(type) {
		case string:
			deltas = append(deltas, typed)
		case map[string]any:
			if text, ok := typed["text"].(string); ok {
				deltas = append(deltas, text)
			}
		}
	})
	return deltas
}

func collectClaudeAssistantText(value any) string {
	message, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	content, ok := message["content"].([]any)
	if !ok {
		return ""
	}

	var builder strings.Builder
	for _, item := range content {
		contentItem, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := contentItem["text"].(string); ok {
			builder.WriteString(text)
		}
	}
	return builder.String()
}

func walkJSON(value any, visit func(string, any)) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			visit(key, child)
			walkJSON(child, visit)
		}
	case []any:
		for _, child := range typed {
			walkJSON(child, visit)
		}
	}
}

func copyStreamOutput(reader io.Reader, stdout *bytes.Buffer, writer StreamWriter) error {
	if flusher, ok := writer.(interface{ Flush() error }); ok {
		defer func() {
			_ = flusher.Flush()
		}()
	}

	buffer := make([]byte, 1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			chunk := string(buffer[:n])
			stdout.WriteString(chunk)
			if writer != nil {
				if writeErr := writer.WriteDelta(chunk); writeErr != nil {
					return writeErr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if isClosedPipeReadError(err) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func isClosedPipeReadError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrClosed) || strings.Contains(err.Error(), "file already closed")
}

func stopProcessGroup(cmd *exec.Cmd, errCh <-chan error) error {
	if cmd.Process == nil {
		return <-errCh
	}

	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-time.After(time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return <-errCh
	}
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

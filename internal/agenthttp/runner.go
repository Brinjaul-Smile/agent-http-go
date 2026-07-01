package agenthttp

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultTimeout 是单次 agent CLI 执行允许的最长时间。
	DefaultTimeout = 10 * time.Minute
	// DefaultAgent 是 /runs 系列接口未显式指定 agent 时使用的后端。
	DefaultAgent = "claude"
)

// RunRequest 表示 /runs 和 /codex 接口接收的 JSON 请求体。
type RunRequest struct {
	// Agent 指定 /runs 使用的后端 runner；为空时默认使用 claude。
	// /codex 兼容接口会忽略该字段。
	Agent string `json:"agent"`
	// Prompt 是传给 agent CLI 的用户输入。
	Prompt string `json:"prompt"`
	// Cwd 是本次 agent 子进程的工作目录，必须位于 WorkspaceRoot 内。
	Cwd string `json:"cwd"`
}

// RunResult 是不同 agent runner 统一返回给 HTTP 层的执行结果。
type RunResult struct {
	// OK 表示 runner 是否成功完成并生成可用结果。
	OK bool
	// Error 是面向 HTTP 调用方的错误摘要。
	Error string
	// ExitCode 是 agent CLI 子进程退出码；未启动或无法获取时为 nil。
	ExitCode *int
	// Output 是归一化后的最终 assistant 文本。
	Output string
	// TimedOut 标记本次执行是否因为超时被终止。
	TimedOut bool
	// Stdout 保存 agent CLI 原始 stdout，仅 debug 响应会返回。
	Stdout string
	// Stderr 保存 agent CLI 原始 stderr，仅 debug 响应会返回。
	Stderr string
	// SessionID 只由持久化会话接口填充；普通 /runs 和 /codex 保持空值。
	SessionID string
}

// Runner 执行一次 agent 请求。
type Runner func(context.Context, RunRequest) (RunResult, error)

// StreamWriter 接收 runner 实时输出的文本片段。
type StreamWriter interface {
	// WriteDelta 写入一段 agent 正文增量。
	WriteDelta(string) error
}

// StreamRunner 执行一次 agent 请求，并尽量把 CLI stdout 的增量片段写给调用方。
type StreamRunner func(context.Context, RunRequest, StreamWriter) (RunResult, error)

// RunnerOptions 注入进程执行所需的工作区、环境变量和超时时间。
type RunnerOptions struct {
	// WorkspaceRoot 限定请求 cwd 可用的根目录。
	WorkspaceRoot string
	// Env 是传给 agent CLI 子进程的环境变量；nil 时使用当前进程环境。
	Env []string
	// Timeout 是单次 agent CLI 执行的最长时间；零值使用 DefaultTimeout。
	Timeout time.Duration
	// CodexCommand 是 codex CLI 命令名或绝对路径。
	CodexCommand string
	// ClaudeCommand 是 claude CLI 命令名或绝对路径。
	ClaudeCommand string
	// CodexAppServerOptions 控制 codex app-server 的 thread/start 运行策略。
	CodexAppServerOptions
}

// CodexAppServerOptions 控制 codex app-server thread/start 的运行策略。
type CodexAppServerOptions struct {
	// ApprovalPolicy 是传给 codex app-server 的审批策略。
	ApprovalPolicy string
	// Sandbox 是传给 codex app-server 的沙箱策略。
	Sandbox string
	// Ephemeral 控制 codex app-server thread 是否为临时线程；nil 表示使用默认 true。
	Ephemeral *bool
}

// ValidatePrompt 校验所有 runner 共享的 prompt 非空规则。
func ValidatePrompt(body RunRequest) (string, error) {
	if strings.TrimSpace(body.Prompt) == "" {
		return "", NewRequestError("prompt must be a non-empty string", http.StatusBadRequest)
	}
	return body.Prompt, nil
}

// requestAgent 返回 /runs 系列接口实际使用的 agent 名称。
func requestAgent(body RunRequest) string {
	if body.Agent == "" {
		return DefaultAgent
	}
	return body.Agent
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

// runnerCommand 返回配置命令名；配置为空时使用 fallback。
func runnerCommand(command string, fallback string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed != "" {
		return trimmed
	}
	return fallback
}

// intString 将退出码转换成响应错误文案需要的字符串。
func intString(value int) string {
	return strconv.Itoa(value)
}

// buildRunResult 将子进程执行结果统一转换为 RunResult，消除各 runner 中
// 「超时 → 进程错误 → 非零退出码 → 成功」的重复判断。
func buildRunResult(child childResult, agentName string, output string) (RunResult, error) {
	if child.TimedOut {
		return RunResult{
			OK:       false,
			Error:    agentName + " execution timed out",
			ExitCode: child.ExitCode,
			TimedOut: true,
			Output:   output,
			Stdout:   child.Stdout,
			Stderr:   child.Stderr,
		}, nil
	}
	if child.Err != nil {
		return RunResult{}, child.Err
	}
	if child.ExitCode == nil || *child.ExitCode != 0 {
		code := 0
		if child.ExitCode != nil {
			code = *child.ExitCode
		}
		return RunResult{
			OK:       false,
			Error:    agentName + " exited with code " + intString(code),
			ExitCode: child.ExitCode,
			Output:   output,
			Stdout:   child.Stdout,
			Stderr:   child.Stderr,
		}, nil
	}

	return RunResult{
		OK:       true,
		ExitCode: child.ExitCode,
		Output:   output,
		Stdout:   child.Stdout,
		Stderr:   child.Stderr,
	}, nil
}

// runnerPrep 保存一次 runner 调用经过校验和解析后的启动参数，
// 消除四个 runner 函数中重复的环境准备、prompt 校验、工作区解析和命令查找逻辑。
type runnerPrep struct {
	// Env 是传给 agent CLI 子进程的环境变量。
	Env []string
	// WorkspaceRoot 是服务配置的工作区根目录。
	WorkspaceRoot string
	// AbsoluteWorkspaceRoot 是 WorkspaceRoot 的绝对路径，供 app-server 等需要使用绝对路径的调用方。
	AbsoluteWorkspaceRoot string
	// Timeout 是单次 agent CLI 执行的最长时间。
	Timeout time.Duration
	// Prompt 是已经校验过的用户输入。
	Prompt string
	// Cwd 是已解析并限制在工作区内的子进程工作目录。
	Cwd string
	// Path 是 agent CLI 可执行文件的解析路径。
	Path string
}

// prepareRunner 集中处理 runner 调用的通用准备步骤：解析环境变量、工作区、超时时间，
// 校验 prompt，解析 cwd 边界，并查找 agent CLI 可执行文件。
func prepareRunner(body RunRequest, options RunnerOptions, command string, fallback string, agentName string) (runnerPrep, error) {
	env := runnerEnv(options.Env)
	workspaceRoot := runnerWorkspaceRoot(options.WorkspaceRoot)
	timeout := runnerTimeout(options.Timeout)

	prompt, err := ValidatePrompt(body)
	if err != nil {
		return runnerPrep{}, err
	}

	cwd, err := ResolveWorkspaceCwd(body.Cwd, workspaceRoot)
	if err != nil {
		return runnerPrep{}, err
	}

	path, err := FindExecutable(runnerCommand(command, fallback), env)
	if err != nil {
		return runnerPrep{}, err
	}
	if path == "" {
		return runnerPrep{}, NewRequestError(agentName+" CLI not found in PATH", http.StatusServiceUnavailable)
	}

	absoluteWorkspaceRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return runnerPrep{}, err
	}

	return runnerPrep{
		Env:                   env,
		WorkspaceRoot:         workspaceRoot,
		AbsoluteWorkspaceRoot: absoluteWorkspaceRoot,
		Timeout:               timeout,
		Prompt:                prompt,
		Cwd:                   cwd,
		Path:                  path,
	}, nil
}

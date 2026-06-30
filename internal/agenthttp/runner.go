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
	CodexCommand  string
	ClaudeCommand string
	CodexAppServerOptions
}

// CodexAppServerOptions 控制 codex app-server thread/start 的运行策略。
type CodexAppServerOptions struct {
	ApprovalPolicy string
	Sandbox        string
	Ephemeral      *bool
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

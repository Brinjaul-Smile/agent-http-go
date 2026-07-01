package agenthttp

import (
	"context"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultSessionMaxTurns 是拼接 prompt 时默认取的最多成功轮数。
	defaultSessionMaxTurns = 20
	// defaultSessionMaxHistoryBytes 是拼接 prompt 时历史文本的默认字节上限。
	defaultSessionMaxHistoryBytes = 64 * 1024
	// maxSessionIDBytes 是 sessionId 允许的最长字节数。
	maxSessionIDBytes = 128
	// defaultSessionListLimit 是 GET /sessions/{sessionId} 默认返回的最近消息条数。
	defaultSessionListLimit = 100

	// SessionRoleUser 表示消息来自用户。
	SessionRoleUser = "user"
	// SessionRoleAssistant 表示消息来自 agent assistant。
	SessionRoleAssistant = "assistant"

	// SessionStatusOK 表示本轮执行成功。
	SessionStatusOK = "ok"
	// SessionStatusFailed 表示本轮执行失败。
	SessionStatusFailed = "failed"
	// SessionStatusTimedOut 表示本轮执行超时。
	SessionStatusTimedOut = "timed_out"
)

// SessionStore 抽象持久会话存储；HTTP 层只依赖这个接口。
// 第一版由 SQLite 实现，后续可增加 MySQL、SQL Server 等 RDS 实现。
type SessionStore interface {
	GetSession(ctx context.Context, id string) (Session, bool, error)
	CreateSession(ctx context.Context, session SessionCreate) (Session, error)
	ListMessages(ctx context.Context, sessionID string, limit int) ([]SessionMessage, error)
	// ListContextMessages 只返回可参与上下文拼接的消息，当前语义是成功 turn。
	ListContextMessages(ctx context.Context, sessionID string, maxMessages int) ([]SessionMessage, error)
	AppendTurn(ctx context.Context, turn SessionTurn) error
	DeleteSession(ctx context.Context, id string) error
}

// Session 保存一段持久化对话的执行边界。
// Agent 和 Cwd 创建后保持不变，避免同一段历史跨 agent 或跨目录复用。
type Session struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`
	Cwd       string    `json:"cwd"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SessionCreate 是创建 session 时需要固定下来的元信息。
type SessionCreate struct {
	ID    string
	Agent string
	Cwd   string
}

// SessionMessage 是 GET /sessions/{sessionId} 返回的可审计消息记录。
// Status 标记这一轮执行结果，失败和超时消息会保存但不参与后续上下文。
type SessionMessage struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"sessionId"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

// SessionTurn 表示一次用户输入和一次 runner 回复的原子写入单元。
type SessionTurn struct {
	SessionID        string
	UserContent      string
	AssistantContent string
	Status           string
}

// SessionRunOptions 控制拼接进 runner prompt 的历史窗口。
type SessionRunOptions struct {
	MaxTurns        int
	MaxHistoryBytes int
}

// normalizedSessionRunOptions 用默认值填充 SessionRunOptions 中的零值字段。
func normalizedSessionRunOptions(options SessionRunOptions) SessionRunOptions {
	if options.MaxTurns <= 0 {
		options.MaxTurns = defaultSessionMaxTurns
	}
	if options.MaxHistoryBytes <= 0 {
		options.MaxHistoryBytes = defaultSessionMaxHistoryBytes
	}
	return options
}

// validateSessionID 校验 sessionId 格式：非空、不超过 128 字节、仅包含字母数字和 . _ - :。
func validateSessionID(input string) (string, error) {
	id := strings.TrimSpace(input)
	if id == "" {
		return "", NewRequestError("sessionId must be a non-empty string", http.StatusBadRequest)
	}
	if len(id) > maxSessionIDBytes {
		return "", NewRequestError("sessionId must be at most 128 bytes", http.StatusBadRequest)
	}
	for _, ch := range id {
		if isSessionIDChar(ch) {
			continue
		}
		return "", NewRequestError("sessionId may contain only letters, numbers, '.', '_', '-' or ':'", http.StatusBadRequest)
	}
	return id, nil
}

// isSessionIDChar 判断字符是否允许出现在 sessionId 中。
func isSessionIDChar(ch rune) bool {
	return ch >= 'a' && ch <= 'z' ||
		ch >= 'A' && ch <= 'Z' ||
		ch >= '0' && ch <= '9' ||
		ch == '.' ||
		ch == '_' ||
		ch == '-' ||
		ch == ':'
}

// buildSessionPrompt 把成功历史包装成单次 CLI prompt。
// runner 仍然是无状态子进程；长对话能力来自这里显式注入 transcript。
func buildSessionPrompt(messages []SessionMessage, latestPrompt string, maxHistoryBytes int) string {
	history := buildSessionHistory(messages, maxHistoryBytes)
	if history == "" {
		return latestPrompt
	}

	var builder strings.Builder
	builder.WriteString("You are continuing a persisted conversation. Use the previous conversation as context and answer the latest user message.\n\n")
	builder.WriteString("Previous conversation:\n")
	builder.WriteString(history)
	builder.WriteString("\nLatest user message:\n")
	builder.WriteString(latestPrompt)
	return builder.String()
}

// buildSessionHistory 从最近的完整 turn 开始向前取，直到达到字节预算。
// 当前用户 prompt 不计入这里的预算，调用方会始终保留最新输入。
func buildSessionHistory(messages []SessionMessage, maxHistoryBytes int) string {
	if maxHistoryBytes <= 0 || len(messages) == 0 {
		return ""
	}

	turns := sessionTurns(messages)
	if len(turns) == 0 {
		return ""
	}

	var selected []string
	totalBytes := 0
	for i := len(turns) - 1; i >= 0; i-- {
		turn := turns[i]
		nextBytes := len(turn)
		if len(selected) > 0 {
			nextBytes += 1
		}
		if totalBytes+nextBytes > maxHistoryBytes {
			break
		}
		selected = append(selected, turn)
		totalBytes += nextBytes
	}

	var builder strings.Builder
	for i := len(selected) - 1; i >= 0; i-- {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(selected[i])
	}
	return builder.String()
}

// sessionTurns 只把相邻的 user/assistant 消息组成完整 turn。
// 如果存储里出现不完整或异常顺序的消息，这里会跳过而不是拼进 prompt。
func sessionTurns(messages []SessionMessage) []string {
	turns := make([]string, 0, len(messages)/2)
	for i := 0; i+1 < len(messages); i++ {
		user := messages[i]
		assistant := messages[i+1]
		if user.Role != SessionRoleUser || assistant.Role != SessionRoleAssistant {
			continue
		}
		var builder strings.Builder
		builder.WriteString("User:\n")
		builder.WriteString(user.Content)
		builder.WriteString("\n\nAssistant:\n")
		builder.WriteString(assistant.Content)
		builder.WriteString("\n")
		turns = append(turns, builder.String())
		i++
	}
	return turns
}

// sessionStatusFromResult 把 runner 结果归一化成存储层状态。
func sessionStatusFromResult(result RunResult) string {
	if result.TimedOut {
		return SessionStatusTimedOut
	}
	if !result.OK {
		return SessionStatusFailed
	}
	return SessionStatusOK
}

// sessionAssistantContent 选择写入 assistant 消息的内容。
// 失败时 Output 通常为空，因此落库错误摘要，便于审计。
func sessionAssistantContent(result RunResult) string {
	if result.Output != "" {
		return result.Output
	}
	return result.Error
}

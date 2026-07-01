package agenthttp

import (
	"context"
	"net/http"
	"strings"
)

// handleSession 分发持久化会话接口：
// POST /sessions/{sessionId}/runs、POST /sessions/{sessionId}/runs/stream、
// GET /sessions/{sessionId}、DELETE /sessions/{sessionId}。
func (s *Server) handleSession(response http.ResponseWriter, request *http.Request) {
	sessionID, action, ok := parseSessionPath(request.URL.Path)
	if !ok {
		handleNotFound(response, request)
		return
	}

	id, err := validateSessionID(sessionID)
	if err != nil {
		s.sendError(response, err)
		return
	}

	switch {
	case request.Method == http.MethodPost && action == "runs_stream":
		s.handleSessionRunStream(response, request, id)
	case request.Method == http.MethodPost && action == "runs":
		s.handleSessionRun(response, request, id)
	case request.Method == http.MethodGet && action == "":
		s.handleGetSession(response, request, id)
	case request.Method == http.MethodDelete && action == "":
		s.handleDeleteSession(response, request, id)
	default:
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
	}
}

// parseSessionPath 从标准库 ServeMux 的 /sessions/ 前缀路由中解析 sessionId 和动作。
// 第一版不允许 sessionId 含斜杠，避免 URL path 编码和路由边界变复杂。
func parseSessionPath(path string) (string, string, bool) {
	rest, ok := strings.CutPrefix(path, "/sessions/")
	if !ok || rest == "" {
		return "", "", false
	}
	if sessionID, ok := strings.CutSuffix(rest, "/runs/stream"); ok {
		if sessionID == "" || strings.Contains(sessionID, "/") {
			return "", "", false
		}
		return sessionID, "runs_stream", true
	}
	if sessionID, ok := strings.CutSuffix(rest, "/runs"); ok {
		if sessionID == "" || strings.Contains(sessionID, "/") {
			return "", "", false
		}
		return sessionID, "runs", true
	}
	if strings.Contains(rest, "/") {
		return "", "", false
	}
	return rest, "", true
}

// handleSessionRun 执行一次持久化会话请求。
// 它在单次无状态 runner 调用外层负责读取历史、拼接 prompt、写回 turn。
func (s *Server) handleSessionRun(response http.ResponseWriter, request *http.Request, sessionID string) {
	body, err := s.readJSONBody(request)
	if err != nil {
		s.sendError(response, err)
		return
	}

	runner, err := s.selectRunner("/runs", body)
	if err != nil {
		s.sendError(response, err)
		return
	}

	prep, err := s.prepareSessionRun(request, body, sessionID)
	if err != nil {
		s.sendError(response, err)
		return
	}
	defer prep.release()

	result, err := runner(request.Context(), prep.body)
	if err != nil {
		if appendErr := s.sessionStore.AppendTurn(request.Context(), SessionTurn{
			SessionID:        sessionID,
			UserContent:      prep.originalPrompt,
			AssistantContent: errorMessage(err),
			Status:           SessionStatusFailed,
		}); appendErr != nil {
			s.logger.Error("failed to append failed session turn", "sessionId", sessionID, "error", appendErr)
		}
		s.sendError(response, err)
		return
	}

	result.SessionID = sessionID
	if err := s.sessionStore.AppendTurn(request.Context(), SessionTurn{
		SessionID:        sessionID,
		UserContent:      prep.originalPrompt,
		AssistantContent: sessionAssistantContent(result),
		Status:           sessionStatusFromResult(result),
	}); err != nil {
		s.sendError(response, err)
		return
	}

	status := http.StatusOK
	if result.TimedOut {
		status = http.StatusGatewayTimeout
	}
	sendJSON(response, status, formatRunResult(result, request.URL.Query().Get("debug") == "1"))
}

// handleSessionRunStream 执行一次持久化会话 SSE 请求。
// 它和同步 session run 共享同样的历史拼接与写回规则，只是响应改为事件流。
func (s *Server) handleSessionRunStream(response http.ResponseWriter, request *http.Request, sessionID string) {
	body, err := s.readJSONBody(request)
	if err != nil {
		s.sendError(response, err)
		return
	}

	runner, err := s.selectStreamRunner("/runs", body)
	if err != nil {
		s.sendError(response, err)
		return
	}

	prep, err := s.prepareSessionRun(request, body, sessionID)
	if err != nil {
		s.sendError(response, err)
		return
	}
	defer prep.release()

	stream, ok := newSSEStream(response)
	if !ok {
		s.sendError(response, NewRequestError("streaming is not supported", http.StatusInternalServerError))
		return
	}
	if err := stream.send("start", map[string]any{"ok": true, "sessionId": sessionID}); err != nil {
		return
	}

	result, err := runner(request.Context(), prep.body, stream)
	if err != nil {
		if appendErr := s.sessionStore.AppendTurn(request.Context(), SessionTurn{
			SessionID:        sessionID,
			UserContent:      prep.originalPrompt,
			AssistantContent: errorMessage(err),
			Status:           SessionStatusFailed,
		}); appendErr != nil {
			s.logger.Error("failed to append failed session turn", "sessionId", sessionID, "error", appendErr)
		}
		if err := stream.send("error", map[string]any{"ok": false, "sessionId": sessionID, "error": errorMessage(err)}); err != nil {
			return
		}
		_ = stream.send("done", map[string]any{"ok": false, "sessionId": sessionID})
		return
	}

	result.SessionID = sessionID
	if err := s.sessionStore.AppendTurn(request.Context(), SessionTurn{
		SessionID:        sessionID,
		UserContent:      prep.originalPrompt,
		AssistantContent: sessionAssistantContent(result),
		Status:           sessionStatusFromResult(result),
	}); err != nil {
		if err := stream.send("error", map[string]any{"ok": false, "sessionId": sessionID, "error": errorMessage(err)}); err != nil {
			return
		}
		_ = stream.send("done", map[string]any{"ok": false, "sessionId": sessionID})
		return
	}

	if err := sendStreamResultIfDebug(stream, result, request.URL.Query().Get("debug") == "1"); err != nil {
		return
	}
	_ = stream.send("done", formatStreamDone(result))
}

// ensureSessionMatches 创建新 session，或校验已存在 session 的 agent/cwd 绑定。
// 一个长对话跨 agent 或跨 cwd 复用历史会让上下文和执行环境不一致。
func (s *Server) ensureSessionMatches(ctx context.Context, sessionID string, agent string, cwd string) error {
	session, ok, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if !ok {
		_, err := s.sessionStore.CreateSession(ctx, SessionCreate{
			ID:    sessionID,
			Agent: agent,
			Cwd:   cwd,
		})
		return err
	}
	if session.Agent != agent {
		return NewRequestError("session already uses agent "+session.Agent, http.StatusBadRequest)
	}
	if session.Cwd != cwd {
		return NewRequestError("session already uses cwd "+session.Cwd, http.StatusBadRequest)
	}
	return nil
}

// handleGetSession 返回 session 元信息和完整消息历史，用于查看和排查持久化状态。
func (s *Server) handleGetSession(response http.ResponseWriter, request *http.Request, sessionID string) {
	session, ok, err := s.sessionStore.GetSession(request.Context(), sessionID)
	if err != nil {
		s.sendError(response, err)
		return
	}
	if !ok {
		sendJSON(response, http.StatusNotFound, map[string]any{"ok": false, "error": "session not found"})
		return
	}

	messages, err := s.sessionStore.ListMessages(request.Context(), sessionID)
	if err != nil {
		s.sendError(response, err)
		return
	}
	sendJSON(response, http.StatusOK, map[string]any{
		"ok":       true,
		"session":  session,
		"messages": messages,
	})
}

// handleDeleteSession 删除 session 及其消息。
// 删除时也获取 session 锁，避免和正在执行的同 session run 并发写删。
func (s *Server) handleDeleteSession(response http.ResponseWriter, request *http.Request, sessionID string) {
	release, err := s.sessionLocks.acquire(request.Context(), sessionID)
	if err != nil {
		s.sendError(response, err)
		return
	}
	defer release()

	if err := s.sessionStore.DeleteSession(request.Context(), sessionID); err != nil {
		s.sendError(response, err)
		return
	}
	sendJSON(response, http.StatusOK, map[string]any{"ok": true})
}

// sessionRunContext 保存一次 session run 经过校验和锁获取后的执行上下文，
// 消除 handleSessionRun 和 handleSessionRunStream 中重复的准备逻辑。
type sessionRunContext struct {
	// body 的 Prompt 已被替换为包含历史上下文的完整 prompt。
	body RunRequest
	// originalPrompt 是请求中原始的用户输入，用于写入 session 消息。
	originalPrompt string
	// release 释放 session 锁。
	release func()
}

// prepareSessionRun 集中处理 session run 的通用准备：
// prompt 校验、cwd 边界解析、session 锁获取、agent/cwd 一致性校验、
// 历史消息查询和 prompt 拼接。
func (s *Server) prepareSessionRun(request *http.Request, body RunRequest, sessionID string) (*sessionRunContext, error) {
	originalPrompt, err := ValidatePrompt(body)
	if err != nil {
		return nil, err
	}
	body.Agent = requestAgent(body)

	cwd, err := ResolveWorkspaceCwd(body.Cwd, s.workspaceRoot)
	if err != nil {
		return nil, err
	}
	body.Cwd = cwd

	release, err := s.sessionLocks.acquire(request.Context(), sessionID)
	if err != nil {
		return nil, err
	}

	if err := s.ensureSessionMatches(request.Context(), sessionID, body.Agent, cwd); err != nil {
		release()
		return nil, err
	}

	messages, err := s.sessionStore.ListContextMessages(request.Context(), sessionID, s.sessionOptions.MaxTurns*2)
	if err != nil {
		release()
		return nil, err
	}
	body.Prompt = buildSessionPrompt(messages, originalPrompt, s.sessionOptions.MaxHistoryBytes)

	return &sessionRunContext{
		body:           body,
		originalPrompt: originalPrompt,
		release:        release,
	}, nil
}

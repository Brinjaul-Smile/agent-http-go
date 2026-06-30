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

	originalPrompt, err := ValidatePrompt(body)
	if err != nil {
		s.sendError(response, err)
		return
	}

	cwd, err := ResolveWorkspaceCwd(body.Cwd, s.workspaceRoot)
	if err != nil {
		s.sendError(response, err)
		return
	}
	body.Cwd = cwd

	// 同一个 session 的历史读、runner 调用、结果写回必须串行，
	// 否则并发请求会基于过期历史生成回复并乱序写入消息。
	release, err := s.sessionLocks.acquire(request.Context(), sessionID)
	if err != nil {
		s.sendError(response, err)
		return
	}
	defer release()

	if err := s.ensureSessionMatches(request.Context(), sessionID, body.Agent, cwd); err != nil {
		s.sendError(response, err)
		return
	}

	// 只取成功消息作为上下文。失败和超时会保留在库里供审计，
	// 但不会进入下一次 prompt，避免把错误输出当成正常对话历史。
	messages, err := s.sessionStore.ListContextMessages(request.Context(), sessionID, s.sessionOptions.MaxTurns*2)
	if err != nil {
		s.sendError(response, err)
		return
	}
	body.Prompt = buildSessionPrompt(messages, originalPrompt, s.sessionOptions.MaxHistoryBytes)

	result, err := runner(request.Context(), body)
	if err != nil {
		// runner 返回执行错误时也记录这一轮，便于后续通过 GET /sessions/{id}
		// 看见用户输入和失败原因；这类 failed turn 不参与后续上下文。
		appendErr := s.sessionStore.AppendTurn(request.Context(), SessionTurn{
			SessionID:        sessionID,
			UserContent:      originalPrompt,
			AssistantContent: errorMessage(err),
			Status:           SessionStatusFailed,
		})
		if appendErr != nil {
			s.sendError(response, appendErr)
			return
		}
		s.sendError(response, err)
		return
	}

	result.SessionID = sessionID
	if err := s.sessionStore.AppendTurn(request.Context(), SessionTurn{
		SessionID:        sessionID,
		UserContent:      originalPrompt,
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

	originalPrompt, err := ValidatePrompt(body)
	if err != nil {
		s.sendError(response, err)
		return
	}

	cwd, err := ResolveWorkspaceCwd(body.Cwd, s.workspaceRoot)
	if err != nil {
		s.sendError(response, err)
		return
	}
	body.Cwd = cwd

	release, err := s.sessionLocks.acquire(request.Context(), sessionID)
	if err != nil {
		s.sendError(response, err)
		return
	}
	defer release()

	if err := s.ensureSessionMatches(request.Context(), sessionID, body.Agent, cwd); err != nil {
		s.sendError(response, err)
		return
	}

	messages, err := s.sessionStore.ListContextMessages(request.Context(), sessionID, s.sessionOptions.MaxTurns*2)
	if err != nil {
		s.sendError(response, err)
		return
	}
	body.Prompt = buildSessionPrompt(messages, originalPrompt, s.sessionOptions.MaxHistoryBytes)

	stream, ok := newSSEStream(response)
	if !ok {
		s.sendError(response, NewRequestError("streaming is not supported", http.StatusInternalServerError))
		return
	}
	stream.send("start", map[string]any{"ok": true, "sessionId": sessionID})

	result, err := runner(request.Context(), body, stream)
	if err != nil {
		appendErr := s.sessionStore.AppendTurn(request.Context(), SessionTurn{
			SessionID:        sessionID,
			UserContent:      originalPrompt,
			AssistantContent: errorMessage(err),
			Status:           SessionStatusFailed,
		})
		if appendErr != nil {
			stream.send("error", map[string]any{"ok": false, "error": errorMessage(appendErr)})
			stream.send("done", map[string]any{"ok": false, "sessionId": sessionID})
			return
		}
		stream.send("error", map[string]any{"ok": false, "sessionId": sessionID, "error": errorMessage(err)})
		stream.send("done", map[string]any{"ok": false, "sessionId": sessionID})
		return
	}

	result.SessionID = sessionID
	if err := s.sessionStore.AppendTurn(request.Context(), SessionTurn{
		SessionID:        sessionID,
		UserContent:      originalPrompt,
		AssistantContent: sessionAssistantContent(result),
		Status:           sessionStatusFromResult(result),
	}); err != nil {
		stream.send("error", map[string]any{"ok": false, "sessionId": sessionID, "error": errorMessage(err)})
		stream.send("done", map[string]any{"ok": false, "sessionId": sessionID})
		return
	}

	sendStreamResultIfDebug(stream, result, request.URL.Query().Get("debug") == "1")
	stream.send("done", formatStreamDone(result))
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

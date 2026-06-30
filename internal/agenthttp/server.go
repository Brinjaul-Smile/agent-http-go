package agenthttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxBodyBytes 限制 JSON 请求体大小，保持和 Node 参考实现一致。
const MaxBodyBytes = 1024 * 1024

// ServerOptions 配置 HTTP 服务，并允许测试注入假的 runner 或 agent 可用性检查。
type ServerOptions struct {
	Runners         map[string]Runner
	StreamRunners   map[string]StreamRunner
	GetAvailability func() ([]AgentStatus, error)
	SessionStore    SessionStore
	SessionOptions  SessionRunOptions
	WorkspaceRoot   string
	Env             []string
	Timeout         time.Duration
	MaxBodyBytes    int64
	LogRoutes       bool
	Logger          *slog.Logger
}

// Server 实现本地 Agent HTTP API。
type Server struct {
	runners         map[string]Runner
	streamRunners   map[string]StreamRunner
	runnerOrder     []string
	getAvailability func() ([]AgentStatus, error)
	sessionStore    SessionStore
	sessionLocks    *sessionLockStore
	sessionOptions  SessionRunOptions
	workspaceRoot   string
	mux             *http.ServeMux
	maxBodyBytes    int64
	logRoutes       bool
	logger          *slog.Logger
}

// NewServer 创建 HTTP handler；未传入 runners 时默认启用 codex 和 claude。
func NewServer(options ServerOptions) *Server {
	env := runnerEnv(options.Env)
	workspaceRoot := runnerWorkspaceRoot(options.WorkspaceRoot)
	timeout := runnerTimeout(options.Timeout)

	runners := options.Runners
	streamRunners := options.StreamRunners
	runnerOrder := make([]string, 0, len(runners))
	if runners == nil {
		runners = map[string]Runner{
			"codex": func(ctx context.Context, request RunRequest) (RunResult, error) {
				return RunCodexContext(ctx, request, RunnerOptions{
					WorkspaceRoot: workspaceRoot,
					Env:           env,
					Timeout:       timeout,
				})
			},
			"claude": func(ctx context.Context, request RunRequest) (RunResult, error) {
				return RunClaudeContext(ctx, request, RunnerOptions{
					WorkspaceRoot: workspaceRoot,
					Env:           env,
					Timeout:       timeout,
				})
			},
		}
		streamRunners = map[string]StreamRunner{
			"codex": func(ctx context.Context, request RunRequest, writer StreamWriter) (RunResult, error) {
				return RunCodexStreamContext(ctx, request, writer, RunnerOptions{
					WorkspaceRoot: workspaceRoot,
					Env:           env,
					Timeout:       timeout,
				})
			},
			"claude": func(ctx context.Context, request RunRequest, writer StreamWriter) (RunResult, error) {
				return RunClaudeStreamContext(ctx, request, writer, RunnerOptions{
					WorkspaceRoot: workspaceRoot,
					Env:           env,
					Timeout:       timeout,
				})
			},
		}
		runnerOrder = []string{"codex", "claude"}
	} else {
		for name := range runners {
			runnerOrder = append(runnerOrder, name)
		}
		sort.Strings(runnerOrder)
		if streamRunners == nil {
			streamRunners = map[string]StreamRunner{}
			for name, runner := range runners {
				runner := runner
				streamRunners[name] = func(ctx context.Context, request RunRequest, _ StreamWriter) (RunResult, error) {
					return runner(ctx, request)
				}
			}
		}
	}

	getAvailability := options.GetAvailability
	if getAvailability == nil {
		getAvailability = func() ([]AgentStatus, error) {
			return GetAgentAvailability(DefaultKnownAgents(), env)
		}
	}

	server := &Server{
		runners:         runners,
		streamRunners:   streamRunners,
		runnerOrder:     runnerOrder,
		getAvailability: getAvailability,
		sessionStore:    options.SessionStore,
		sessionOptions:  normalizedSessionRunOptions(options.SessionOptions),
		workspaceRoot:   workspaceRoot,
		maxBodyBytes:    serverMaxBodyBytes(options.MaxBodyBytes),
		logRoutes:       options.LogRoutes,
		logger:          options.Logger,
	}
	if server.sessionStore != nil {
		server.sessionLocks = newSessionLockStore()
	}
	if server.logger == nil {
		server.logger = slog.Default()
	}
	server.mux = server.routes()

	return server
}

// routes 使用标准库 ServeMux 注册 API 路由。
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	s.registerRoute(mux, http.MethodGet, "/health", "agenthttp.(*Server).handleHealth", s.handleHealth)
	s.registerRoute(mux, http.MethodGet, "/agents", "agenthttp.(*Server).handleAgents", s.handleAgents)
	s.registerRoute(mux, http.MethodPost, "/codex", "agenthttp.(*Server).handleRun", s.handleRun)
	s.registerRoute(mux, http.MethodPost, "/codex/stream", "agenthttp.(*Server).handleRunStream", s.handleRunStream)
	s.registerRoute(mux, http.MethodPost, "/runs", "agenthttp.(*Server).handleRun", s.handleRun)
	s.registerRoute(mux, http.MethodPost, "/runs/stream", "agenthttp.(*Server).handleRunStream", s.handleRunStream)
	if s.sessionStore != nil {
		s.registerRoute(mux, http.MethodGet, "/sessions/{sessionId}", "agenthttp.(*Server).handleSession", s.handleSession)
		s.logRegisteredRoute(http.MethodDelete, "/sessions/{sessionId}", "agenthttp.(*Server).handleSession")
		s.logRegisteredRoute(http.MethodPost, "/sessions/{sessionId}/runs", "agenthttp.(*Server).handleSession")
		s.logRegisteredRoute(http.MethodPost, "/sessions/{sessionId}/runs/stream", "agenthttp.(*Server).handleSession")
	}
	mux.HandleFunc("/", handleNotFound)
	return mux
}

// registerRoute 注册路由，并在配置开启时输出类似 Gin 的路由注册日志。
func (s *Server) registerRoute(mux *http.ServeMux, method string, path string, handler string, fn http.HandlerFunc) {
	if strings.Contains(path, "{") {
		mux.HandleFunc("/sessions/", fn)
	} else {
		mux.HandleFunc(path, fn)
	}
	s.logRegisteredRoute(method, path, handler)
}

// logRegisteredRoute 在开启路由日志时输出注册信息。
// 对共享同一个 ServeMux 前缀的 session 路由，也按对外 API 路径分别打印。
func (s *Server) logRegisteredRoute(method string, path string, handler string) {
	if !s.logRoutes {
		return
	}
	s.logger.Info("registered route", "method", method, "path", path, "handler", handler)
}

// ServeHTTP 将请求交给标准库 ServeMux 分发。
func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	s.mux.ServeHTTP(response, request)
}

// handleHealth 返回服务健康状态。
func (s *Server) handleHealth(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handleNotFound(response, request)
		return
	}
	sendJSON(response, http.StatusOK, map[string]any{"ok": true})
}

// handleAgents 返回本机已知 agent CLI 的可用性。
func (s *Server) handleAgents(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handleNotFound(response, request)
		return
	}

	agents, err := s.getAvailability()
	if err != nil {
		sendJSON(response, http.StatusInternalServerError, map[string]any{"ok": false, "error": errorMessage(err)})
		return
	}
	sendJSON(response, http.StatusOK, map[string]any{"ok": true, "agents": agents})
}

// handleNotFound 返回统一 JSON 404，避免 ServeMux 默认返回纯文本错误。
func handleNotFound(response http.ResponseWriter, _ *http.Request) {
	sendJSON(response, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
}

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

// handleRun 同时处理 POST /runs 和兼容接口 POST /codex。
func (s *Server) handleRun(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}

	body, err := s.readJSONBody(request)
	if err != nil {
		s.sendError(response, err)
		return
	}

	runner, err := s.selectRunner(request.URL.Path, body)
	if err != nil {
		s.sendError(response, err)
		return
	}

	result, err := runner(request.Context(), body)
	if err != nil {
		s.sendError(response, err)
		return
	}

	status := http.StatusOK
	if result.TimedOut {
		status = http.StatusGatewayTimeout
	}
	sendJSON(response, status, formatRunResult(result, request.URL.Query().Get("debug") == "1"))
}

// handleRunStream 处理无持久化历史的 SSE 运行接口。
// StreamRunner 会把 CLI stdout 片段推成 delta；默认只用 done 返回最终状态。
func (s *Server) handleRunStream(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}

	body, err := s.readJSONBody(request)
	if err != nil {
		s.sendError(response, err)
		return
	}

	runnerPath := "/runs"
	if request.URL.Path == "/codex/stream" {
		runnerPath = "/codex"
	}

	runner, err := s.selectStreamRunner(runnerPath, body)
	if err != nil {
		s.sendError(response, err)
		return
	}

	stream, ok := newSSEStream(response)
	if !ok {
		s.sendError(response, NewRequestError("streaming is not supported", http.StatusInternalServerError))
		return
	}
	stream.send("start", map[string]any{"ok": true})

	result, err := runner(request.Context(), body, stream)
	if err != nil {
		stream.send("error", map[string]any{"ok": false, "error": errorMessage(err)})
		stream.send("done", map[string]any{"ok": false})
		return
	}

	sendStreamResultIfDebug(stream, result, request.URL.Query().Get("debug") == "1")
	stream.send("done", formatStreamDone(result))
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

// selectRunner 将 /codex 固定映射到 codex runner，将 /runs 映射到请求中的 agent。
func (s *Server) selectRunner(path string, body RunRequest) (Runner, error) {
	if path == "/codex" {
		runner, ok := s.runners["codex"]
		if !ok {
			return nil, NewRequestError("agent must be one of: "+strings.Join(s.runnerNames(), ", "), http.StatusBadRequest)
		}
		return runner, nil
	}

	runner, ok := s.runners[body.Agent]
	if !ok {
		return nil, NewRequestError("agent must be one of: "+strings.Join(s.runnerNames(), ", "), http.StatusBadRequest)
	}
	return runner, nil
}

// selectStreamRunner 将流式接口映射到对应 stream runner。
// /codex/stream 固定使用 codex；/runs/stream 使用请求体中的 agent。
func (s *Server) selectStreamRunner(path string, body RunRequest) (StreamRunner, error) {
	if path == "/codex" {
		runner, ok := s.streamRunners["codex"]
		if !ok {
			return nil, NewRequestError("agent must be one of: "+strings.Join(s.runnerNames(), ", "), http.StatusBadRequest)
		}
		return runner, nil
	}

	runner, ok := s.streamRunners[body.Agent]
	if !ok {
		return nil, NewRequestError("agent must be one of: "+strings.Join(s.runnerNames(), ", "), http.StatusBadRequest)
	}
	return runner, nil
}

// runnerNames 返回当前服务支持的 runner 名称，用于生成错误提示。
func (s *Server) runnerNames() []string {
	names := append([]string(nil), s.runnerOrder...)
	return names
}

// sendError 根据错误类型选择 HTTP 状态码并返回统一 JSON 错误体。
func (s *Server) sendError(response http.ResponseWriter, err error) {
	var requestErr *RequestError
	if errors.As(err, &requestErr) {
		sendJSON(response, requestErr.StatusCode, map[string]any{"ok": false, "error": requestErr.Message})
		return
	}
	sendJSON(response, http.StatusInternalServerError, map[string]any{"ok": false, "error": errorMessage(err)})
}

// readJSONBody 保持 Node 参考实现的行为：空请求体会变成空请求对象，
// prompt 等业务校验交给 runner 处理。
func (s *Server) readJSONBody(request *http.Request) (RunRequest, error) {
	defer request.Body.Close()

	decoder := json.NewDecoder(http.MaxBytesReader(nil, request.Body, s.maxBodyBytes))
	var body RunRequest
	if err := decoder.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return RunRequest{}, nil
		}
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return RunRequest{}, NewRequestError("request body too large", http.StatusRequestEntityTooLarge)
		}
		return RunRequest{}, NewRequestError("invalid JSON body", http.StatusBadRequest)
	}
	return body, nil
}

// formatRunResult 默认隐藏原始 stdout/stderr；只有调用方显式传 debug=1 时才返回。
func formatRunResult(result RunResult, includeDebug bool) map[string]any {
	response := map[string]any{"ok": result.OK}
	if result.SessionID != "" {
		response["sessionId"] = result.SessionID
	}
	if result.Error != "" {
		response["error"] = result.Error
	}
	if result.ExitCode != nil {
		response["exitCode"] = *result.ExitCode
	}
	if result.Output != "" || !result.OK {
		response["output"] = result.Output
	}
	if result.TimedOut {
		response["timedOut"] = true
	}
	if includeDebug {
		response["debug"] = map[string]any{
			"stdout": result.Stdout,
			"stderr": result.Stderr,
		}
	}
	return response
}

func sendStreamResultIfDebug(stream *sseStream, result RunResult, includeDebug bool) {
	if !includeDebug {
		return
	}
	stream.send("result", formatRunResult(result, true))
}

func formatStreamDone(result RunResult) map[string]any {
	response := map[string]any{"ok": result.OK}
	if result.SessionID != "" {
		response["sessionId"] = result.SessionID
	}
	if result.Error != "" {
		response["error"] = result.Error
	}
	if result.ExitCode != nil {
		response["exitCode"] = *result.ExitCode
	}
	if result.TimedOut {
		response["timedOut"] = true
	}
	return response
}

// sendJSON 写出统一的 JSON 响应头和响应体。
func sendJSON(response http.ResponseWriter, statusCode int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		statusCode = http.StatusInternalServerError
		payload = []byte(`{"ok":false,"error":"internal server error"}`)
	}

	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	response.WriteHeader(statusCode)
	_, _ = response.Write(payload)
}

type sseStream struct {
	response http.ResponseWriter
	flusher  http.Flusher
}

// newSSEStream 初始化 Server-Sent Events 响应头。
// SSE 仍基于 HTTP，适合服务端单向推送 start/delta/done 这类 chat 事件。
func newSSEStream(response http.ResponseWriter) (*sseStream, bool) {
	flusher, ok := response.(http.Flusher)
	if !ok {
		return nil, false
	}

	header := response.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	flusher.Flush()

	return &sseStream{response: response, flusher: flusher}, true
}

// send 写出一个 SSE 事件。data 使用 JSON，方便客户端按事件类型解析结构化载荷。
func (s *sseStream) send(event string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		payload = []byte(`{"ok":false,"error":"internal server error"}`)
	}

	_, _ = s.response.Write([]byte("event: " + event + "\n"))
	for _, line := range strings.Split(string(payload), "\n") {
		_, _ = s.response.Write([]byte("data: " + line + "\n"))
	}
	_, _ = s.response.Write([]byte("\n"))
	s.flusher.Flush()
}

// WriteDelta 实现 StreamWriter，把 runner stdout 片段转成 SSE delta 事件。
func (s *sseStream) WriteDelta(delta string) error {
	s.send("delta", map[string]any{"delta": delta})
	return nil
}

// errorMessage 返回非空错误文案，避免给调用方返回空字符串。
func errorMessage(err error) string {
	if err == nil {
		return "internal server error"
	}
	if err.Error() == "" {
		return "internal server error"
	}
	return err.Error()
}

func serverMaxBodyBytes(maxBodyBytes int64) int64 {
	if maxBodyBytes > 0 {
		return maxBodyBytes
	}
	return MaxBodyBytes
}

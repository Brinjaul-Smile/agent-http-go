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
	GetAvailability func() ([]AgentStatus, error)
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
	runnerOrder     []string
	getAvailability func() ([]AgentStatus, error)
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
		runnerOrder = []string{"codex", "claude"}
	} else {
		for name := range runners {
			runnerOrder = append(runnerOrder, name)
		}
		sort.Strings(runnerOrder)
	}

	getAvailability := options.GetAvailability
	if getAvailability == nil {
		getAvailability = func() ([]AgentStatus, error) {
			return GetAgentAvailability(DefaultKnownAgents(), env)
		}
	}

	server := &Server{
		runners:         runners,
		runnerOrder:     runnerOrder,
		getAvailability: getAvailability,
		maxBodyBytes:    serverMaxBodyBytes(options.MaxBodyBytes),
		logRoutes:       options.LogRoutes,
		logger:          options.Logger,
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
	s.registerRoute(mux, http.MethodPost, "/runs", "agenthttp.(*Server).handleRun", s.handleRun)
	mux.HandleFunc("/", handleNotFound)
	return mux
}

// registerRoute 注册路由，并在配置开启时输出类似 Gin 的路由注册日志。
func (s *Server) registerRoute(mux *http.ServeMux, method string, path string, handler string, fn http.HandlerFunc) {
	mux.HandleFunc(path, fn)
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

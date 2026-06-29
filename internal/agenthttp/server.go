package agenthttp

import (
	"encoding/json"
	"errors"
	"io"
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
}

// Server 实现本地 Agent HTTP API。
type Server struct {
	runners         map[string]Runner
	runnerOrder     []string
	getAvailability func() ([]AgentStatus, error)
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
			"codex": func(request RunRequest) (RunResult, error) {
				return RunCodex(request, RunnerOptions{
					WorkspaceRoot: workspaceRoot,
					Env:           env,
					Timeout:       timeout,
				})
			},
			"claude": func(request RunRequest) (RunResult, error) {
				return RunClaude(request, RunnerOptions{
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

	return &Server{
		runners:         runners,
		runnerOrder:     runnerOrder,
		getAvailability: getAvailability,
	}
}

// ServeHTTP 定义 API 路由表。
func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/health":
		sendJSON(response, http.StatusOK, map[string]any{"ok": true})
	case request.Method == http.MethodGet && request.URL.Path == "/agents":
		s.handleAgents(response)
	case request.URL.Path == "/codex" || request.URL.Path == "/runs":
		s.handleRun(response, request)
	default:
		sendJSON(response, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
	}
}

// handleAgents 返回本机已知 agent CLI 的可用性。
func (s *Server) handleAgents(response http.ResponseWriter) {
	agents, err := s.getAvailability()
	if err != nil {
		sendJSON(response, http.StatusInternalServerError, map[string]any{"ok": false, "error": errorMessage(err)})
		return
	}
	sendJSON(response, http.StatusOK, map[string]any{"ok": true, "agents": agents})
}

// handleRun 同时处理 POST /runs 和兼容接口 POST /codex。
func (s *Server) handleRun(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}

	body, err := readJSONBody(request)
	if err != nil {
		s.sendError(response, err)
		return
	}

	runner, err := s.selectRunner(request.URL.Path, body)
	if err != nil {
		s.sendError(response, err)
		return
	}

	result, err := runner(body)
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
func readJSONBody(request *http.Request) (RunRequest, error) {
	defer request.Body.Close()

	decoder := json.NewDecoder(http.MaxBytesReader(nil, request.Body, MaxBodyBytes))
	var body RunRequest
	if err := decoder.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return RunRequest{}, nil
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

package agenthttp

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
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
	CodexCommand    string
	ClaudeCommand   string
	CodexAppServer  CodexAppServerOptions
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
					CodexCommand:  options.CodexCommand,
				})
			},
			"claude": func(ctx context.Context, request RunRequest) (RunResult, error) {
				return RunClaudeContext(ctx, request, RunnerOptions{
					WorkspaceRoot: workspaceRoot,
					Env:           env,
					Timeout:       timeout,
					ClaudeCommand: options.ClaudeCommand,
				})
			},
		}
		streamRunners = map[string]StreamRunner{
			"codex": func(ctx context.Context, request RunRequest, writer StreamWriter) (RunResult, error) {
				return RunCodexStreamContext(ctx, request, writer, RunnerOptions{
					WorkspaceRoot:         workspaceRoot,
					Env:                   env,
					Timeout:               timeout,
					CodexCommand:          options.CodexCommand,
					CodexAppServerOptions: options.CodexAppServer,
				})
			},
			"claude": func(ctx context.Context, request RunRequest, writer StreamWriter) (RunResult, error) {
				return RunClaudeStreamContext(ctx, request, writer, RunnerOptions{
					WorkspaceRoot: workspaceRoot,
					Env:           env,
					Timeout:       timeout,
					ClaudeCommand: options.ClaudeCommand,
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
		s.registerRoute(mux, http.MethodGet, "/examples/session-stream", "agenthttp.(*Server).handleSessionStreamExample", s.handleSessionStreamExample)
		s.registerRoute(mux, http.MethodGet, "/examples/session-stream/", "agenthttp.(*Server).handleSessionStreamExample", s.handleSessionStreamExample)
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

// serverMaxBodyBytes 返回有效的请求体大小上限；未配置时使用默认值。
func serverMaxBodyBytes(maxBodyBytes int64) int64 {
	if maxBodyBytes > 0 {
		return maxBodyBytes
	}
	return MaxBodyBytes
}

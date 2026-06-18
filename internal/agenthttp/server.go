package agenthttp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const MaxBodyBytes = 1024 * 1024

type ServerOptions struct {
	Runners         map[string]Runner
	GetAvailability func() ([]AgentStatus, error)
	WorkspaceRoot   string
	Env             []string
	Timeout         time.Duration
}

type Server struct {
	runners         map[string]Runner
	runnerOrder     []string
	getAvailability func() ([]AgentStatus, error)
}

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
		sortStrings(runnerOrder)
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

func (s *Server) handleAgents(response http.ResponseWriter) {
	agents, err := s.getAvailability()
	if err != nil {
		sendJSON(response, http.StatusInternalServerError, map[string]any{"ok": false, "error": errorMessage(err)})
		return
	}
	sendJSON(response, http.StatusOK, map[string]any{"ok": true, "agents": agents})
}

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

func (s *Server) runnerNames() []string {
	names := append([]string(nil), s.runnerOrder...)
	return names
}

func (s *Server) sendError(response http.ResponseWriter, err error) {
	var requestErr *RequestError
	if errors.As(err, &requestErr) {
		sendJSON(response, requestErr.StatusCode, map[string]any{"ok": false, "error": requestErr.Message})
		return
	}
	sendJSON(response, http.StatusInternalServerError, map[string]any{"ok": false, "error": errorMessage(err)})
}

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

func errorMessage(err error) string {
	if err == nil {
		return "internal server error"
	}
	if err.Error() == "" {
		return "internal server error"
	}
	return err.Error()
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		value := values[i]
		j := i - 1
		for j >= 0 && values[j] > value {
			values[j+1] = values[j]
			j--
		}
		values[j+1] = value
	}
}

func ListenAndServe(host string, port string, options ServerOptions) error {
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "8787"
	}
	if options.Env == nil {
		options.Env = os.Environ()
	}
	return http.ListenAndServe(host+":"+port, NewServer(options))
}

package agenthttp

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGETHealthReturnsOK(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(RunRequest) (RunResult, error) {
				return RunResult{OK: true}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	assertJSON(t, response, map[string]any{"ok": true})
}

func TestGETAgentsReturnsAgentAvailability(t *testing.T) {
	server := NewServer(ServerOptions{
		GetAvailability: func() ([]AgentStatus, error) {
			return []AgentStatus{
				{Name: "codex", Command: "codex", Available: true, Supported: true},
				{Name: "claude", Command: "claude", Available: false, Supported: true, Error: "claude CLI not found in PATH"},
			}, nil
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/agents", nil)
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	assertJSON(t, response, map[string]any{
		"ok": true,
		"agents": []any{
			map[string]any{"name": "codex", "command": "codex", "available": true, "supported": true},
			map[string]any{"name": "claude", "command": "claude", "available": false, "supported": true, "error": "claude CLI not found in PATH"},
		},
	})
}

func TestPOSTCodexReturnsRunnerResult(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(request RunRequest) (RunResult, error) {
				exitCode := 0
				return RunResult{
					OK:       true,
					ExitCode: &exitCode,
					Output:   "received:" + request.Prompt,
					Stdout:   "stdout",
					Stderr:   "stderr",
				}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/codex", jsonBody(t, map[string]any{"prompt": "hello"}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	assertJSON(t, response, map[string]any{
		"ok":       true,
		"exitCode": float64(0),
		"output":   "received:hello",
	})
}

func TestPOSTRunsDispatchesToRequestedRunner(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("codex runner should not be called")
			},
			"claude": func(request RunRequest) (RunResult, error) {
				exitCode := 0
				return RunResult{
					OK:       true,
					ExitCode: &exitCode,
					Output:   "claude:" + request.Prompt,
					Stdout:   "stdout",
					Stderr:   "stderr",
				}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/runs", jsonBody(t, map[string]any{"agent": "claude", "prompt": "hello"}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	assertJSON(t, response, map[string]any{
		"ok":       true,
		"exitCode": float64(0),
		"output":   "claude:hello",
	})
}

func TestPOSTRunsRejectsUnknownAgent(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(RunRequest) (RunResult, error) {
				return RunResult{OK: true}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/runs", jsonBody(t, map[string]any{"agent": "missing", "prompt": "hello"}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusBadRequest)
	assertJSON(t, response, map[string]any{
		"ok":    false,
		"error": "agent must be one of: codex",
	})
}

func TestPOSTRunsDefaultRunnerListIncludesClaudeInNodeOrder(t *testing.T) {
	server := NewServer(ServerOptions{})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/runs", jsonBody(t, map[string]any{"agent": "missing", "prompt": "hello"}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusBadRequest)
	assertJSON(t, response, map[string]any{
		"ok":    false,
		"error": "agent must be one of: codex, claude",
	})
}

func TestPOSTCodexMapsMissingCLIErrorsTo503(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(RunRequest) (RunResult, error) {
				return RunResult{}, NewRequestError("codex CLI not found in PATH", http.StatusServiceUnavailable)
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/codex", jsonBody(t, map[string]any{"prompt": "hello"}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusServiceUnavailable)
	assertJSON(t, response, map[string]any{
		"ok":    false,
		"error": "codex CLI not found in PATH",
	})
}

func TestPOSTCodexReturnsDebugOutputWhenRequested(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(request RunRequest) (RunResult, error) {
				exitCode := 0
				return RunResult{
					OK:       true,
					ExitCode: &exitCode,
					Output:   "received:" + request.Prompt,
					Stdout:   "stdout",
					Stderr:   "stderr",
				}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/codex?debug=1", jsonBody(t, map[string]any{"prompt": "hello"}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	assertJSON(t, response, map[string]any{
		"ok":       true,
		"exitCode": float64(0),
		"output":   "received:hello",
		"debug": map[string]any{
			"stdout": "stdout",
			"stderr": "stderr",
		},
	})
}

func TestPOSTCodexFormatsFailedRunnerResultWithoutDebugByDefault(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(RunRequest) (RunResult, error) {
				exitCode := 7
				return RunResult{
					OK:       false,
					Error:    "codex exited with code 7",
					ExitCode: &exitCode,
					Output:   "",
					Stdout:   "stdout",
					Stderr:   "stderr",
				}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/codex", jsonBody(t, map[string]any{"prompt": "hello"}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	assertJSON(t, response, map[string]any{
		"ok":       false,
		"error":    "codex exited with code 7",
		"exitCode": float64(7),
		"output":   "",
	})
}

func TestPOSTCodexReturns504ForTimedOutRunnerResult(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(RunRequest) (RunResult, error) {
				return RunResult{OK: false, Error: "codex execution timed out", TimedOut: true, Output: ""}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/codex", jsonBody(t, map[string]any{"prompt": "hello"}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusGatewayTimeout)
	assertJSON(t, response, map[string]any{
		"ok":       false,
		"error":    "codex execution timed out",
		"output":   "",
		"timedOut": true,
	})
}

func TestPOSTCodexAllowsEmptyBodyToReachRunnerValidation(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(request RunRequest) (RunResult, error) {
				_, err := ValidatePrompt(request)
				return RunResult{}, err
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/codex", bytes.NewReader(nil))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusBadRequest)
	assertJSON(t, response, map[string]any{
		"ok":    false,
		"error": "prompt must be a non-empty string",
	})
}

func TestPOSTCodexReturns400ForInvalidJSON(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(RunRequest) (RunResult, error) {
				return RunResult{OK: true}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/codex", bytes.NewBufferString("{"))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusBadRequest)
	assertJSON(t, response, map[string]any{
		"ok":    false,
		"error": "invalid JSON body",
	})
}

func TestUnknownRoutesReturn404(t *testing.T) {
	server := NewServer(ServerOptions{})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusNotFound)
	assertJSON(t, response, map[string]any{"ok": false, "error": "not found"})
}

func TestUnsupportedCodexMethodsReturn405(t *testing.T) {
	server := NewServer(ServerOptions{})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/codex", nil)
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusMethodNotAllowed)
	assertJSON(t, response, map[string]any{"ok": false, "error": "method not allowed"})
}

func jsonBody(t *testing.T, body map[string]any) *bytes.Reader {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(payload)
}

func assertStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()

	if response.Code != want {
		t.Fatalf("status = %d, want %d, body = %s", response.Code, want, response.Body.String())
	}
}

func assertJSON(t *testing.T, response *httptest.ResponseRecorder, want map[string]any) {
	t.Helper()

	var got map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(got, want) {
		t.Fatalf("json = %#v, want %#v", got, want)
	}
}

func jsonEqual(a, b any) bool {
	encodedA, _ := json.Marshal(a)
	encodedB, _ := json.Marshal(b)
	return bytes.Equal(encodedA, encodedB)
}

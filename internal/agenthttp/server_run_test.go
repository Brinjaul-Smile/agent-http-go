package agenthttp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPOSTCodexReturnsRunnerResult(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(_ context.Context, request RunRequest) (RunResult, error) {
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
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("codex runner should not be called")
			},
			"claude": func(_ context.Context, request RunRequest) (RunResult, error) {
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
			"codex": func(context.Context, RunRequest) (RunResult, error) {
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
			"codex": func(context.Context, RunRequest) (RunResult, error) {
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
			"codex": func(_ context.Context, request RunRequest) (RunResult, error) {
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
			"codex": func(context.Context, RunRequest) (RunResult, error) {
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
			"codex": func(context.Context, RunRequest) (RunResult, error) {
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
			"codex": func(_ context.Context, request RunRequest) (RunResult, error) {
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
			"codex": func(context.Context, RunRequest) (RunResult, error) {
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

func TestPOSTCodexReturns413WhenBodyIsTooLarge(t *testing.T) {
	server := NewServer(ServerOptions{
		MaxBodyBytes: 8,
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{OK: true}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/codex", bytes.NewBufferString(`{"prompt":"hello"}`))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusRequestEntityTooLarge)
	assertJSON(t, response, map[string]any{
		"ok":    false,
		"error": "request body too large",
	})
}

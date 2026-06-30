package agenthttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGETHealthReturnsOK(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
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

func TestNewServerLogsRegisteredRoutesWhenEnabled(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	_ = NewServer(ServerOptions{
		LogRoutes: true,
		Logger:    logger,
	})

	output := logs.String()
	for _, expected := range []string{
		"method=GET path=/health",
		"method=GET path=/agents",
		"method=POST path=/codex",
		"method=POST path=/runs",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("route log missing %q in:\n%s", expected, output)
		}
	}
}

func TestNewServerDoesNotLogRegisteredRoutesByDefault(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	_ = NewServer(ServerOptions{
		Logger: logger,
	})

	if logs.Len() != 0 {
		t.Fatalf("logs = %q, want empty", logs.String())
	}
}

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

func TestPOSTRunsStreamReturnsSSEEvents(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("sync runner should not be called")
			},
		},
		StreamRunners: map[string]StreamRunner{
			"codex": func(_ context.Context, request RunRequest, writer StreamWriter) (RunResult, error) {
				if err := writer.WriteDelta("chunk-1:"); err != nil {
					return RunResult{}, err
				}
				if err := writer.WriteDelta(request.Prompt); err != nil {
					return RunResult{}, err
				}
				exitCode := 0
				return RunResult{OK: true, ExitCode: &exitCode, Output: "streamed:" + request.Prompt}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/runs/stream", jsonBody(t, map[string]any{
		"agent":  "codex",
		"prompt": "hello",
	}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	if contentType := response.Header().Get("Content-Type"); contentType != "text/event-stream; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/event-stream", contentType)
	}
	body := response.Body.String()
	for _, expected := range []string{
		"event: start\n",
		"event: delta\n",
		`"delta":"chunk-1:"`,
		`"delta":"hello"`,
		"event: done\n",
		`"exitCode":0`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("SSE body missing %q in:\n%s", expected, body)
		}
	}
	for _, unexpected := range []string{
		"event: result\n",
		`"output":"streamed:hello"`,
	} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("SSE body unexpectedly contains %q in:\n%s", unexpected, body)
		}
	}
}

func TestPOSTRunStreamDebugIncludesResult(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("sync runner should not be called")
			},
		},
		StreamRunners: map[string]StreamRunner{
			"codex": func(_ context.Context, request RunRequest, writer StreamWriter) (RunResult, error) {
				if err := writer.WriteDelta(request.Prompt); err != nil {
					return RunResult{}, err
				}
				exitCode := 0
				return RunResult{
					OK:       true,
					ExitCode: &exitCode,
					Output:   "streamed:" + request.Prompt,
					Stdout:   "raw stdout",
					Stderr:   "raw stderr",
				}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/runs/stream?debug=1", jsonBody(t, map[string]any{
		"agent":  "codex",
		"prompt": "hello",
	}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	body := response.Body.String()
	for _, expected := range []string{
		"event: result\n",
		`"output":"streamed:hello"`,
		`"stdout":"raw stdout"`,
		`"stderr":"raw stderr"`,
		"event: done\n",
		`"exitCode":0`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("SSE body missing %q in:\n%s", expected, body)
		}
	}
}

func TestPOSTCodexStreamUsesCodexRunner(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("sync runner should not be called")
			},
			"claude": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("claude runner should not be called")
			},
		},
		StreamRunners: map[string]StreamRunner{
			"codex": func(_ context.Context, request RunRequest, writer StreamWriter) (RunResult, error) {
				if err := writer.WriteDelta("codex:"); err != nil {
					return RunResult{}, err
				}
				exitCode := 0
				return RunResult{OK: true, ExitCode: &exitCode, Output: "codex-streamed:" + request.Prompt}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/codex/stream", jsonBody(t, map[string]any{
		"prompt": "hello",
	}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	body := response.Body.String()
	for _, expected := range []string{
		"event: start\n",
		"event: delta\n",
		`"delta":"codex:"`,
		"event: done\n",
		`"exitCode":0`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("SSE body missing %q in:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "event: result\n") {
		t.Fatalf("SSE body unexpectedly contains result event:\n%s", body)
	}
}

func TestPOSTSessionRunKeepsSuccessfulConversationHistory(t *testing.T) {
	workspaceRoot := t.TempDir()
	store := newMemorySessionStore()
	var prompts []string
	server := NewServer(ServerOptions{
		WorkspaceRoot: workspaceRoot,
		SessionStore:  store,
		Runners: map[string]Runner{
			"codex": func(_ context.Context, request RunRequest) (RunResult, error) {
				prompts = append(prompts, request.Prompt)
				output := "answer-1"
				if len(prompts) == 2 {
					output = "answer-2"
				}
				exitCode := 0
				return RunResult{OK: true, ExitCode: &exitCode, Output: output}, nil
			},
		},
	})

	firstResponse := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs", jsonBody(t, map[string]any{
		"agent":  "codex",
		"prompt": "hello",
	}))
	server.ServeHTTP(firstResponse, firstRequest)

	assertStatus(t, firstResponse, http.StatusOK)
	assertJSON(t, firstResponse, map[string]any{
		"ok":        true,
		"sessionId": "chat-1",
		"exitCode":  float64(0),
		"output":    "answer-1",
	})

	secondResponse := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs", jsonBody(t, map[string]any{
		"agent":  "codex",
		"prompt": "what next?",
	}))
	server.ServeHTTP(secondResponse, secondRequest)

	assertStatus(t, secondResponse, http.StatusOK)
	assertJSON(t, secondResponse, map[string]any{
		"ok":        true,
		"sessionId": "chat-1",
		"exitCode":  float64(0),
		"output":    "answer-2",
	})

	if len(prompts) != 2 {
		t.Fatalf("prompts len = %d, want 2", len(prompts))
	}
	if prompts[0] != "hello" {
		t.Fatalf("first prompt = %q, want original prompt", prompts[0])
	}
	for _, expected := range []string{
		"Previous conversation:",
		"User:\nhello",
		"Assistant:\nanswer-1",
		"Latest user message:\nwhat next?",
	} {
		if !strings.Contains(prompts[1], expected) {
			t.Fatalf("second prompt missing %q in:\n%s", expected, prompts[1])
		}
	}
}

func TestPOSTSessionRunStreamKeepsHistoryAndWritesSession(t *testing.T) {
	workspaceRoot := t.TempDir()
	store := newMemorySessionStore()
	var prompts []string
	server := NewServer(ServerOptions{
		WorkspaceRoot: workspaceRoot,
		SessionStore:  store,
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("sync runner should not be called")
			},
		},
		StreamRunners: map[string]StreamRunner{
			"codex": func(_ context.Context, request RunRequest, writer StreamWriter) (RunResult, error) {
				prompts = append(prompts, request.Prompt)
				if err := writer.WriteDelta("delta-" + strconv.Itoa(len(prompts))); err != nil {
					return RunResult{}, err
				}
				output := "answer-1"
				if len(prompts) == 2 {
					output = "answer-2"
				}
				exitCode := 0
				return RunResult{OK: true, ExitCode: &exitCode, Output: output}, nil
			},
		},
	})

	firstResponse := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs/stream", jsonBody(t, map[string]any{
		"agent":  "codex",
		"prompt": "hello",
	}))
	server.ServeHTTP(firstResponse, firstRequest)

	assertStatus(t, firstResponse, http.StatusOK)
	if !strings.Contains(firstResponse.Body.String(), `"sessionId":"chat-1"`) {
		t.Fatalf("first SSE body missing sessionId:\n%s", firstResponse.Body.String())
	}

	secondResponse := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs/stream", jsonBody(t, map[string]any{
		"agent":  "codex",
		"prompt": "what next?",
	}))
	server.ServeHTTP(secondResponse, secondRequest)

	assertStatus(t, secondResponse, http.StatusOK)
	body := secondResponse.Body.String()
	for _, expected := range []string{
		"event: start\n",
		"event: delta\n",
		`"delta":"delta-2"`,
		"event: done\n",
		`"exitCode":0`,
		`"sessionId":"chat-1"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("second SSE body missing %q in:\n%s", expected, body)
		}
	}
	for _, unexpected := range []string{
		"event: result\n",
		`"output":"answer-2"`,
	} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("second SSE body unexpectedly contains %q in:\n%s", unexpected, body)
		}
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts len = %d, want 2", len(prompts))
	}
	if !strings.Contains(prompts[1], "User:\nhello") || !strings.Contains(prompts[1], "Latest user message:\nwhat next?") {
		t.Fatalf("second prompt did not include history:\n%s", prompts[1])
	}

	messages, err := store.ListMessages(context.Background(), "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages len = %d, want 4", len(messages))
	}
}

func TestPOSTSessionRunRejectsAgentMismatch(t *testing.T) {
	workspaceRoot := t.TempDir()
	server := NewServer(ServerOptions{
		WorkspaceRoot: workspaceRoot,
		SessionStore:  newMemorySessionStore(),
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{OK: true, Output: "ok"}, nil
			},
			"claude": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{OK: true, Output: "ok"}, nil
			},
		},
	})

	firstResponse := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs", jsonBody(t, map[string]any{
		"agent":  "codex",
		"prompt": "hello",
	}))
	server.ServeHTTP(firstResponse, firstRequest)
	assertStatus(t, firstResponse, http.StatusOK)

	secondResponse := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs", jsonBody(t, map[string]any{
		"agent":  "claude",
		"prompt": "hello",
	}))
	server.ServeHTTP(secondResponse, secondRequest)

	assertStatus(t, secondResponse, http.StatusBadRequest)
	assertJSON(t, secondResponse, map[string]any{
		"ok":    false,
		"error": "session already uses agent codex",
	})
}

func TestPOSTSessionRunStoresFailedTurnButSkipsItFromContext(t *testing.T) {
	workspaceRoot := t.TempDir()
	store := newMemorySessionStore()
	var prompts []string
	server := NewServer(ServerOptions{
		WorkspaceRoot: workspaceRoot,
		SessionStore:  store,
		Runners: map[string]Runner{
			"codex": func(_ context.Context, request RunRequest) (RunResult, error) {
				prompts = append(prompts, request.Prompt)
				if len(prompts) == 1 {
					return RunResult{OK: false, Error: "codex exited with code 7"}, nil
				}
				return RunResult{OK: true, Output: "recovered"}, nil
			},
		},
	})

	firstResponse := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs", jsonBody(t, map[string]any{
		"agent":  "codex",
		"prompt": "break",
	}))
	server.ServeHTTP(firstResponse, firstRequest)

	assertStatus(t, firstResponse, http.StatusOK)
	assertJSON(t, firstResponse, map[string]any{
		"ok":        false,
		"sessionId": "chat-1",
		"error":     "codex exited with code 7",
		"output":    "",
	})

	secondResponse := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs", jsonBody(t, map[string]any{
		"agent":  "codex",
		"prompt": "retry?",
	}))
	server.ServeHTTP(secondResponse, secondRequest)

	assertStatus(t, secondResponse, http.StatusOK)
	if prompts[1] != "retry?" {
		t.Fatalf("second prompt = %q, want failed history skipped", prompts[1])
	}

	messages, err := store.ListMessages(context.Background(), "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages len = %d, want 4", len(messages))
	}
	if messages[0].Status != SessionStatusFailed || messages[1].Status != SessionStatusFailed {
		t.Fatalf("failed turn statuses = %q/%q, want failed", messages[0].Status, messages[1].Status)
	}
}

func TestGETAndDELETESession(t *testing.T) {
	workspaceRoot := t.TempDir()
	server := NewServer(ServerOptions{
		WorkspaceRoot: workspaceRoot,
		SessionStore:  newMemorySessionStore(),
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{OK: true, Output: "answer"}, nil
			},
		},
	})

	runResponse := httptest.NewRecorder()
	runRequest := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs", jsonBody(t, map[string]any{
		"agent":  "codex",
		"prompt": "hello",
	}))
	server.ServeHTTP(runResponse, runRequest)
	assertStatus(t, runResponse, http.StatusOK)

	getResponse := httptest.NewRecorder()
	getRequest := httptest.NewRequest(http.MethodGet, "/sessions/chat-1", nil)
	server.ServeHTTP(getResponse, getRequest)
	assertStatus(t, getResponse, http.StatusOK)

	var body map[string]any
	if err := json.Unmarshal(getResponse.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %#v, want true", body["ok"])
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %#v, want array", body["messages"])
	}
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}

	deleteResponse := httptest.NewRecorder()
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/sessions/chat-1", nil)
	server.ServeHTTP(deleteResponse, deleteRequest)
	assertStatus(t, deleteResponse, http.StatusOK)
	assertJSON(t, deleteResponse, map[string]any{"ok": true})

	missingResponse := httptest.NewRecorder()
	missingRequest := httptest.NewRequest(http.MethodGet, "/sessions/chat-1", nil)
	server.ServeHTTP(missingResponse, missingRequest)
	assertStatus(t, missingResponse, http.StatusNotFound)
	assertJSON(t, missingResponse, map[string]any{"ok": false, "error": "session not found"})
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

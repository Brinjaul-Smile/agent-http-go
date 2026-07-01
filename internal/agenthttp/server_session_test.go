package agenthttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

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

	messages, err := store.ListMessages(context.Background(), "chat-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages len = %d, want 4", len(messages))
	}
}

func TestPOSTSessionRunStreamWithClaudeDoesNotExposeFinalResultAsSSE(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake command is POSIX-only")
	}

	workspaceRoot := t.TempDir()
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "claude", fakeClaudeStreamScript())
	store := newMemorySessionStore()
	server := NewServer(ServerOptions{
		WorkspaceRoot: workspaceRoot,
		Env:           envWithPath(binDir),
		Timeout:       5 * time.Second,
		SessionStore:  store,
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/sessions/chat-claude/runs/stream", jsonBody(t, map[string]any{
		"agent":  "claude",
		"prompt": "hello",
	}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	body := response.Body.String()
	for _, expected := range []string{
		"event: start\n",
		"event: delta\n",
		`"delta":"stream:"`,
		`"delta":"hello"`,
		"event: done\n",
		`"sessionId":"chat-claude"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("SSE body missing %q in:\n%s", expected, body)
		}
	}
	for _, unexpected := range []string{
		"event: result\n",
		`"output":"final:hello"`,
		`"delta":"final:hello"`,
	} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("SSE body unexpectedly contains %q in:\n%s", unexpected, body)
		}
	}

	messages, err := store.ListMessages(context.Background(), "chat-claude", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	if messages[1].Role != SessionRoleAssistant || messages[1].Content != "final:hello" {
		t.Fatalf("assistant message = %#v, want persisted final result", messages[1])
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

func TestPOSTSessionRunDefaultsToClaudeAgent(t *testing.T) {
	workspaceRoot := t.TempDir()
	store := newMemorySessionStore()
	server := NewServer(ServerOptions{
		WorkspaceRoot: workspaceRoot,
		SessionStore:  store,
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("codex runner should not be called")
			},
			"claude": func(_ context.Context, request RunRequest) (RunResult, error) {
				return RunResult{OK: true, Output: "claude:" + request.Prompt}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs", jsonBody(t, map[string]any{
		"prompt": "hello",
	}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	session, ok, err := store.GetSession(context.Background(), "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("session was not created")
	}
	if session.Agent != DefaultAgent {
		t.Fatalf("session agent = %q, want %q", session.Agent, DefaultAgent)
	}
}

func TestPOSTSessionRunStreamDefaultsToClaudeAgent(t *testing.T) {
	workspaceRoot := t.TempDir()
	store := newMemorySessionStore()
	server := NewServer(ServerOptions{
		WorkspaceRoot: workspaceRoot,
		SessionStore:  store,
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("sync codex runner should not be called")
			},
			"claude": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("sync claude runner should not be called")
			},
		},
		StreamRunners: map[string]StreamRunner{
			"codex": func(context.Context, RunRequest, StreamWriter) (RunResult, error) {
				return RunResult{}, errors.New("codex stream runner should not be called")
			},
			"claude": func(_ context.Context, request RunRequest, writer StreamWriter) (RunResult, error) {
				if request.Agent != DefaultAgent {
					return RunResult{}, errors.New("session stream request should default to claude")
				}
				if err := writer.WriteDelta("claude:" + request.Prompt); err != nil {
					return RunResult{}, err
				}
				return RunResult{OK: true, Output: "claude:" + request.Prompt}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs/stream", jsonBody(t, map[string]any{
		"prompt": "hello",
	}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	if !strings.Contains(response.Body.String(), `"delta":"claude:hello"`) {
		t.Fatalf("SSE body missing claude delta:\n%s", response.Body.String())
	}
	session, ok, err := store.GetSession(context.Background(), "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("session was not created")
	}
	if session.Agent != DefaultAgent {
		t.Fatalf("session agent = %q, want %q", session.Agent, DefaultAgent)
	}
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

	messages, err := store.ListMessages(context.Background(), "chat-1", 0)
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

func TestGETSessionDefaultsToRecentMessagesAndSupportsLimitAll(t *testing.T) {
	workspaceRoot := t.TempDir()
	store := newMemorySessionStore()
	server := NewServer(ServerOptions{
		WorkspaceRoot: workspaceRoot,
		SessionStore:  store,
		Runners: map[string]Runner{
			"codex": func(_ context.Context, request RunRequest) (RunResult, error) {
				return RunResult{OK: true, Output: "answer:" + request.Prompt}, nil
			},
		},
	})

	for i := 0; i < 60; i++ {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/sessions/chat-1/runs", jsonBody(t, map[string]any{
			"agent":  "codex",
			"prompt": "turn-" + strconv.Itoa(i),
		}))
		server.ServeHTTP(response, request)
		assertStatus(t, response, http.StatusOK)
	}

	defaultResponse := httptest.NewRecorder()
	defaultRequest := httptest.NewRequest(http.MethodGet, "/sessions/chat-1", nil)
	server.ServeHTTP(defaultResponse, defaultRequest)
	assertStatus(t, defaultResponse, http.StatusOK)
	defaultMessages := sessionMessagesFromResponse(t, defaultResponse)
	if len(defaultMessages) != defaultSessionListLimit {
		t.Fatalf("default messages len = %d, want %d", len(defaultMessages), defaultSessionListLimit)
	}

	allResponse := httptest.NewRecorder()
	allRequest := httptest.NewRequest(http.MethodGet, "/sessions/chat-1?limit=all", nil)
	server.ServeHTTP(allResponse, allRequest)
	assertStatus(t, allResponse, http.StatusOK)
	allMessages := sessionMessagesFromResponse(t, allResponse)
	if len(allMessages) != 120 {
		t.Fatalf("all messages len = %d, want 120", len(allMessages))
	}

	limitedResponse := httptest.NewRecorder()
	limitedRequest := httptest.NewRequest(http.MethodGet, "/sessions/chat-1?limit=3", nil)
	server.ServeHTTP(limitedResponse, limitedRequest)
	assertStatus(t, limitedResponse, http.StatusOK)
	limitedMessages := sessionMessagesFromResponse(t, limitedResponse)
	if len(limitedMessages) != 3 {
		t.Fatalf("limited messages len = %d, want 3", len(limitedMessages))
	}
}

func TestGETSessionRejectsInvalidLimit(t *testing.T) {
	server := NewServer(ServerOptions{
		SessionStore: newMemorySessionStore(),
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

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/sessions/chat-1?limit=nope", nil)
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusBadRequest)
	assertJSON(t, response, map[string]any{
		"ok":    false,
		"error": "limit must be a positive integer or all",
	})
}

func sessionMessagesFromResponse(t *testing.T, response *httptest.ResponseRecorder) []any {
	t.Helper()

	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %#v, want array", body["messages"])
	}
	return messages
}

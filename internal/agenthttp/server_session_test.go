package agenthttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
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

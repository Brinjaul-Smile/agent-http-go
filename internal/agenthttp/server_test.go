package agenthttp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func TestUnknownRoutesReturn404(t *testing.T) {
	server := NewServer(ServerOptions{})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusNotFound)
	assertJSON(t, response, map[string]any{"ok": false, "error": "not found"})
}

func TestGETSessionStreamExampleRequiresSessionStore(t *testing.T) {
	server := NewServer(ServerOptions{})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/examples/session-stream", nil)
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusNotFound)
	assertJSON(t, response, map[string]any{"ok": false, "error": "not found"})
}

func TestGETSessionStreamExampleReturnsHTMLWhenSessionsAreEnabled(t *testing.T) {
	server := NewServer(ServerOptions{
		SessionStore: newMemorySessionStore(),
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/examples/session-stream", nil)
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	if contentType := response.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/html", contentType)
	}
	if !strings.Contains(response.Body.String(), "会话流式调试台") {
		t.Fatalf("example HTML missing title:\n%s", response.Body.String())
	}
}

func TestGETSessionStreamExampleRedirectsTrailingSlash(t *testing.T) {
	server := NewServer(ServerOptions{
		SessionStore: newMemorySessionStore(),
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/examples/session-stream/", nil)
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusMovedPermanently)
	if location := response.Header().Get("Location"); location != "/examples/session-stream" {
		t.Fatalf("location = %q, want /examples/session-stream", location)
	}
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

func assertDeprecatedEndpoint(t *testing.T, response *httptest.ResponseRecorder, successor string) {
	t.Helper()

	if deprecated := response.Header().Get("Deprecation"); deprecated != "true" {
		t.Fatalf("Deprecation header = %q, want true", deprecated)
	}
	wantLink := "<" + successor + ">; rel=\"successor-version\""
	if link := response.Header().Get("Link"); link != wantLink {
		t.Fatalf("Link header = %q, want %q", link, wantLink)
	}
}

func jsonEqual(a, b any) bool {
	encodedA, _ := json.Marshal(a)
	encodedB, _ := json.Marshal(b)
	return bytes.Equal(encodedA, encodedB)
}

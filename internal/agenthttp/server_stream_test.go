package agenthttp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func TestPOSTRunsStreamDefaultsToClaudeRunner(t *testing.T) {
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("sync runner should not be called")
			},
		},
		StreamRunners: map[string]StreamRunner{
			"codex": func(context.Context, RunRequest, StreamWriter) (RunResult, error) {
				return RunResult{}, errors.New("codex stream runner should not be called")
			},
			"claude": func(_ context.Context, request RunRequest, writer StreamWriter) (RunResult, error) {
				if err := writer.WriteDelta("claude:" + request.Prompt); err != nil {
					return RunResult{}, err
				}
				exitCode := 0
				return RunResult{OK: true, ExitCode: &exitCode}, nil
			},
		},
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/runs/stream", jsonBody(t, map[string]any{
		"prompt": "hello",
	}))
	server.ServeHTTP(response, request)

	assertStatus(t, response, http.StatusOK)
	if body := response.Body.String(); !strings.Contains(body, `"delta":"claude:hello"`) {
		t.Fatalf("SSE body missing claude delta:\n%s", body)
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
	assertDeprecatedEndpoint(t, response, "/runs/stream")
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

func TestPOSTRunsStreamPropagatesSSEWriteError(t *testing.T) {
	writeErr := errors.New("client disconnected")
	var runnerWriteErr error
	server := NewServer(ServerOptions{
		Runners: map[string]Runner{
			"codex": func(context.Context, RunRequest) (RunResult, error) {
				return RunResult{}, errors.New("sync runner should not be called")
			},
		},
		StreamRunners: map[string]StreamRunner{
			"codex": func(_ context.Context, _ RunRequest, writer StreamWriter) (RunResult, error) {
				runnerWriteErr = writer.WriteDelta("chunk after disconnect")
				return RunResult{}, runnerWriteErr
			},
		},
	})

	response := &failingSSEWriter{
		header:    make(http.Header),
		failAfter: 3,
		err:       writeErr,
	}
	request := httptest.NewRequest(http.MethodPost, "/runs/stream", jsonBody(t, map[string]any{
		"agent":  "codex",
		"prompt": "hello",
	}))
	server.ServeHTTP(response, request)

	if !errors.Is(runnerWriteErr, writeErr) {
		t.Fatalf("runner write error = %v, want %v", runnerWriteErr, writeErr)
	}
	if response.statusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.statusCode, http.StatusOK)
	}
	if body := response.body.String(); strings.Contains(body, "chunk after disconnect") {
		t.Fatalf("response body contains failed delta:\n%s", body)
	}
}

type failingSSEWriter struct {
	header     http.Header
	body       bytes.Buffer
	err        error
	statusCode int
	writes     int
	failAfter  int
}

func (w *failingSSEWriter) Header() http.Header {
	return w.header
}

func (w *failingSSEWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *failingSSEWriter) Write(payload []byte) (int, error) {
	if w.writes >= w.failAfter {
		return 0, w.err
	}
	w.writes++
	return w.body.Write(payload)
}

func (w *failingSSEWriter) Flush() {}

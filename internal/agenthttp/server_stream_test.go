package agenthttp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestPOSTRunsStreamFlushesClaudeDeltaBeforeRunnerCompletes(t *testing.T) {
	allowFinish := make(chan struct{})
	var allowFinishOnce sync.Once
	finishRunner := func() {
		allowFinishOnce.Do(func() {
			close(allowFinish)
		})
	}
	deltaWritten := make(chan struct{})
	runnerReturned := make(chan struct{})
	exitCode := 0

	server := NewServer(ServerOptions{
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
			"claude": func(ctx context.Context, request RunRequest, writer StreamWriter) (RunResult, error) {
				defer close(runnerReturned)
				if request.Agent != "" {
					return RunResult{}, errors.New("request should omit agent and default to claude")
				}
				if err := writer.WriteDelta("first:"); err != nil {
					return RunResult{}, err
				}
				close(deltaWritten)
				select {
				case <-allowFinish:
				case <-ctx.Done():
					return RunResult{}, ctx.Err()
				}
				if err := writer.WriteDelta("second"); err != nil {
					return RunResult{}, err
				}
				return RunResult{OK: true, ExitCode: &exitCode, Output: "first:second:final"}, nil
			},
		},
	})
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/runs/stream", strings.NewReader(`{"prompt":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	defer finishRunner()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	reader := bufio.NewReader(response.Body)
	startBlock := readSSEBlockWithin(t, reader)
	if !strings.Contains(startBlock, "event: start\n") {
		t.Fatalf("first SSE block = %q, want start event", startBlock)
	}

	deltaBlock := readSSEBlockWithin(t, reader)
	if !strings.Contains(deltaBlock, "event: delta\n") || !strings.Contains(deltaBlock, `"delta":"first:"`) {
		t.Fatalf("second SSE block = %q, want first delta", deltaBlock)
	}
	select {
	case <-deltaWritten:
	default:
		t.Fatal("delta block was read before the claude stream runner reported writing it")
	}
	select {
	case <-runnerReturned:
		t.Fatal("runner returned before the test released it; stream may be buffered until final output")
	default:
	}

	finishRunner()
	secondDeltaBlock := readSSEBlockWithin(t, reader)
	doneBlock := readSSEBlockWithin(t, reader)
	combined := deltaBlock + secondDeltaBlock + doneBlock
	for _, expected := range []string{
		`"delta":"second"`,
		"event: done\n",
		`"exitCode":0`,
	} {
		if !strings.Contains(combined, expected) {
			t.Fatalf("SSE stream missing %q in:\n%s", expected, combined)
		}
	}
	for _, unexpected := range []string{
		"event: result\n",
		`"output":"first:second:final"`,
	} {
		if strings.Contains(combined, unexpected) {
			t.Fatalf("SSE stream unexpectedly contains %q in:\n%s", unexpected, combined)
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

func readSSEBlockWithin(t *testing.T, reader *bufio.Reader) string {
	t.Helper()

	type readResult struct {
		block string
		err   error
	}
	results := make(chan readResult, 1)
	go func() {
		var block strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				results <- readResult{err: err}
				return
			}
			block.WriteString(line)
			if line == "\n" || line == "\r\n" {
				results <- readResult{block: block.String()}
				return
			}
		}
	}()

	select {
	case result := <-results:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.block
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SSE block")
	}
	return ""
}

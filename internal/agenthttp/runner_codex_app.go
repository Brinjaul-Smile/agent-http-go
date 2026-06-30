package agenthttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type appServerLine struct {
	line string
	err  error
}

type appServerMessage struct {
	ID     any             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *appServerError `json:"error,omitempty"`
}

type appServerError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type appServerRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type appServerResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *appServerError `json:"error,omitempty"`
}

type appServerThreadStartResult struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
}

type appServerTurnStartResult struct {
	Turn struct {
		ID string `json:"id"`
	} `json:"turn"`
}

type appServerDeltaParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type appServerTurnCompletedParams struct {
	ThreadID string        `json:"threadId"`
	Turn     appServerTurn `json:"turn"`
}

type appServerTurn struct {
	ID     string              `json:"id"`
	Status string              `json:"status"`
	Error  *appServerTurnError `json:"error"`
	Items  []appServerItem     `json:"items"`
}

type appServerTurnError struct {
	Message string `json:"message"`
}

type appServerItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func runCodexAppServerStream(ctx context.Context, prompt string, timeout time.Duration, spec execCommandSpec, writer StreamWriter, cwd string, workspaceRoot string, appOptions CodexAppServerOptions) codexAppServerRun {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(spec.Name, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = spec.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return codexAppServerRun{childResult: childResult{Err: err}}
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return codexAppServerRun{childResult: childResult{Err: err}}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return codexAppServerRun{childResult: childResult{Err: err}}
	}

	if err := ctx.Err(); err != nil {
		return codexAppServerRun{childResult: childResult{Err: err}}
	}
	if err := cmd.Start(); err != nil {
		return codexAppServerRun{childResult: childResult{Err: err}}
	}

	var (
		stdoutMu sync.Mutex
		stdout   bytes.Buffer
		stderr   bytes.Buffer
	)
	lineCh := make(chan appServerLine, 1)
	stderrDone := make(chan error, 1)
	waitCh := make(chan error, 1)

	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			stdoutMu.Lock()
			stdout.WriteString(line)
			stdout.WriteByte('\n')
			stdoutMu.Unlock()
			lineCh <- appServerLine{line: line}
		}
		if err := scanner.Err(); err != nil && !isClosedPipeReadError(err) {
			lineCh <- appServerLine{err: err}
		}
		close(lineCh)
	}()
	go func() {
		_, err := io.Copy(&stderr, stderrPipe)
		stderrDone <- err
	}()
	go func() {
		waitCh <- cmd.Wait()
	}()

	result := codexAppServerRun{}
	sendRequest := func(id int, method string, params any) error {
		payload, err := json.Marshal(appServerRequest{
			JSONRPC: "2.0",
			ID:      id,
			Method:  method,
			Params:  params,
		})
		if err != nil {
			return err
		}
		if _, err := stdinPipe.Write(append(payload, '\n')); err != nil {
			return err
		}
		return nil
	}
	sendResponseError := func(id any, code int, message string) error {
		payload, err := json.Marshal(appServerResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &appServerError{
				Code:    code,
				Message: message,
			},
		})
		if err != nil {
			return err
		}
		if _, err := stdinPipe.Write(append(payload, '\n')); err != nil {
			return err
		}
		return nil
	}

	if err := sendRequest(1, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "agent-http-go",
			"title":   nil,
			"version": "0",
		},
		"capabilities": map[string]any{
			"experimentalApi":    true,
			"requestAttestation": false,
		},
	}); err != nil {
		result.Err = err
		_ = stopProcessGroup(cmd, waitCh)
		return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, nil)
	}

	var (
		threadID     string
		turnID       string
		deltaTracker = newAppServerDeltaTracker()
	)

	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, nil)
			}
			if line.err != nil {
				result.Err = line.err
				_ = stopProcessGroup(cmd, waitCh)
				return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, nil)
			}
			if err := handleCodexAppServerLine(line.line, writer, sendRequest, sendResponseError, prompt, cwd, workspaceRoot, appOptions, &threadID, &turnID, deltaTracker, &result); err != nil {
				result.Err = err
				_ = stopProcessGroup(cmd, waitCh)
				return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, nil)
			}
			if result.Completed {
				_ = stdinPipe.Close()
			}
		case err := <-waitCh:
			for line := range lineCh {
				if line.err != nil {
					if result.Err == nil {
						result.Err = line.err
					}
					continue
				}
				if handleErr := handleCodexAppServerLine(line.line, writer, sendRequest, sendResponseError, prompt, cwd, workspaceRoot, appOptions, &threadID, &turnID, deltaTracker, &result); handleErr != nil && result.Err == nil {
					result.Err = handleErr
				}
			}
			return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, err)
		case <-ctx.Done():
			waitErr := stopProcessGroup(cmd, waitCh)
			if ctx.Err() == context.DeadlineExceeded {
				result.TimedOut = true
			} else {
				result.Err = ctx.Err()
			}
			return finishCodexAppServerRun(result, cmd, waitCh, stderrDone, &stdoutMu, &stdout, &stderr, waitErr)
		}
	}
}

func handleCodexAppServerLine(line string, writer StreamWriter, sendRequest func(int, string, any) error, sendResponseError func(any, int, string) error, prompt string, cwd string, workspaceRoot string, appOptions CodexAppServerOptions, threadID *string, turnID *string, deltaTracker *appServerDeltaTracker, result *codexAppServerRun) error {
	var message appServerMessage
	if err := json.Unmarshal([]byte(line), &message); err != nil {
		return nil
	}
	if message.Error != nil {
		if message.Error.Message != "" {
			return NewRequestError("codex app-server error: "+message.Error.Message, http.StatusBadGateway)
		}
		return NewRequestError("codex app-server returned an error", http.StatusBadGateway)
	}

	if id, ok := jsonNumberID(message.ID); ok {
		switch id {
		case 1:
			result.Initialized = true
			return sendRequest(2, "thread/start", map[string]any{
				"cwd":                   cwd,
				"runtimeWorkspaceRoots": []string{workspaceRoot},
				"approvalPolicy":        appOptions.ApprovalPolicy,
				"sandbox":               appOptions.Sandbox,
				"ephemeral":             codexAppServerEphemeral(appOptions),
			})
		case 2:
			var payload appServerThreadStartResult
			if err := json.Unmarshal(message.Result, &payload); err != nil {
				return err
			}
			*threadID = payload.Thread.ID
			return sendRequest(3, "turn/start", map[string]any{
				"threadId": *threadID,
				"input": []map[string]any{{
					"type":          "text",
					"text":          prompt,
					"text_elements": []any{},
				}},
				"cwd":                   cwd,
				"runtimeWorkspaceRoots": []string{workspaceRoot},
			})
		case 3:
			var payload appServerTurnStartResult
			if err := json.Unmarshal(message.Result, &payload); err == nil {
				*turnID = payload.Turn.ID
			}
			return nil
		}
		if message.Method != "" {
			return sendResponseError(message.ID, -32000, "server request is not supported by agent-http-go")
		}
	}

	switch message.Method {
	case "item/agentMessage/delta":
		var payload appServerDeltaParams
		if err := json.Unmarshal(message.Params, &payload); err != nil {
			return nil
		}
		if payload.ThreadID != *threadID {
			return nil
		}
		if *turnID != "" && payload.TurnID != *turnID {
			return nil
		}
		if payload.Delta == "" || writer == nil {
			return nil
		}
		delta := deltaTracker.Normalize(payload)
		if delta == "" {
			return nil
		}
		return writer.WriteDelta(delta)
	case "turn/completed":
		var payload appServerTurnCompletedParams
		if err := json.Unmarshal(message.Params, &payload); err != nil {
			return nil
		}
		if payload.ThreadID != *threadID {
			return nil
		}
		if *turnID != "" && payload.Turn.ID != "" && payload.Turn.ID != *turnID {
			return nil
		}
		result.Completed = true
		result.TurnStatus = payload.Turn.Status
		if payload.Turn.Error != nil {
			result.TurnError = payload.Turn.Error.Message
		}
		result.Output = lastAgentMessageText(payload.Turn.Items)
	}

	return nil
}

type appServerDeltaTracker struct {
	seen map[string]string
}

func newAppServerDeltaTracker() *appServerDeltaTracker {
	return &appServerDeltaTracker{seen: map[string]string{}}
}

func (t *appServerDeltaTracker) Normalize(payload appServerDeltaParams) string {
	key := payload.ItemID
	if key == "" {
		key = payload.TurnID
	}
	if key == "" {
		key = payload.ThreadID
	}

	delta, next := normalizeTextDelta(t.seen[key], payload.Delta)
	t.seen[key] = next
	return delta
}

func finishCodexAppServerRun(result codexAppServerRun, cmd *exec.Cmd, waitCh <-chan error, stderrDone <-chan error, stdoutMu *sync.Mutex, stdout *bytes.Buffer, stderr *bytes.Buffer, waitErr error) codexAppServerRun {
	if waitErr == nil && cmd.ProcessState == nil {
		waitErr = <-waitCh
	}
	if err := <-stderrDone; err != nil && !isClosedPipeReadError(err) && result.Err == nil {
		result.Err = err
	}

	stdoutMu.Lock()
	result.Stdout = stdout.String()
	stdoutMu.Unlock()
	result.Stderr = stderr.String()

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code := exitErr.ExitCode()
		result.ExitCode = &code
		return result
	}
	if waitErr != nil && result.Err == nil {
		result.Err = waitErr
		return result
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.ExitCode = &code
	}
	return result
}

func jsonNumberID(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case string:
		id, err := strconv.Atoi(typed)
		return id, err == nil
	default:
		return 0, false
	}
}

func lastAgentMessageText(items []appServerItem) string {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == "agentMessage" {
			return items[i].Text
		}
	}
	return ""
}

func normalizedCodexAppServerOptions(options CodexAppServerOptions) CodexAppServerOptions {
	if strings.TrimSpace(options.ApprovalPolicy) == "" {
		options.ApprovalPolicy = "never"
	}
	if strings.TrimSpace(options.Sandbox) == "" {
		options.Sandbox = "workspace-write"
	}
	return options
}

func codexAppServerEphemeral(options CodexAppServerOptions) bool {
	if options.Ephemeral == nil {
		return true
	}
	return *options.Ephemeral
}

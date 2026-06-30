package agenthttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// appServerLine 表示从 codex app-server stdout 扫描到的一行 JSON-RPC 消息或读取错误。
type appServerLine struct {
	// line 是 app-server stdout 中的一行文本。
	line string
	// err 是扫描 stdout 时遇到的读取错误。
	err error
}

// appServerMessage 是 codex app-server JSON-RPC 消息的通用包络。
type appServerMessage struct {
	// ID 是 JSON-RPC 请求或响应 ID，可能是数字或字符串。
	ID any `json:"id,omitempty"`
	// Method 是 JSON-RPC request/notification 方法名。
	Method string `json:"method,omitempty"`
	// Params 是 request/notification 的原始参数 JSON。
	Params json.RawMessage `json:"params,omitempty"`
	// Result 是 response 的原始结果 JSON。
	Result json.RawMessage `json:"result,omitempty"`
	// Error 是 response 的 JSON-RPC 错误对象。
	Error *appServerError `json:"error,omitempty"`
}

// appServerError 表示 codex app-server JSON-RPC error 对象。
type appServerError struct {
	// Code 是 JSON-RPC 错误码。
	Code int `json:"code,omitempty"`
	// Message 是 JSON-RPC 错误文案。
	Message string `json:"message,omitempty"`
}

// appServerRequest 表示 agent-http-go 主动发给 codex app-server 的 JSON-RPC 请求。
type appServerRequest struct {
	// JSONRPC 固定为 "2.0"。
	JSONRPC string `json:"jsonrpc"`
	// ID 是本端分配的请求序号。
	ID int `json:"id"`
	// Method 是 app-server 方法名。
	Method string `json:"method"`
	// Params 是方法参数对象。
	Params any `json:"params"`
}

// appServerResponse 表示 agent-http-go 回复 codex app-server server request 的 JSON-RPC 响应。
type appServerResponse struct {
	// JSONRPC 固定为 "2.0"。
	JSONRPC string `json:"jsonrpc"`
	// ID 必须复用 app-server 请求里的 ID。
	ID any `json:"id"`
	// Result 是成功响应载荷；当前未使用。
	Result any `json:"result,omitempty"`
	// Error 是不支持的 server request 响应错误。
	Error *appServerError `json:"error,omitempty"`
}

// appServerThreadStartResult 是 thread/start 响应结果。
type appServerThreadStartResult struct {
	// Thread 保存 app-server 创建出的线程元信息。
	Thread struct {
		// ID 是后续 turn/start 和事件过滤使用的 threadId。
		ID string `json:"id"`
	} `json:"thread"`
}

// appServerTurnStartResult 是 turn/start 响应结果。
type appServerTurnStartResult struct {
	// Turn 保存 app-server 创建出的 turn 元信息。
	Turn struct {
		// ID 是后续 delta 和 completed 事件过滤使用的 turnId。
		ID string `json:"id"`
	} `json:"turn"`
}

// appServerDeltaParams 是 item/agentMessage/delta 通知参数。
type appServerDeltaParams struct {
	// ThreadID 标识 delta 所属 thread。
	ThreadID string `json:"threadId"`
	// TurnID 标识 delta 所属 turn。
	TurnID string `json:"turnId"`
	// ItemID 标识 delta 所属 agent message item。
	ItemID string `json:"itemId"`
	// Delta 是 app-server 输出的文本片段，可能是纯增量或累计文本。
	Delta string `json:"delta"`
}

// appServerTurnCompletedParams 是 turn/completed 通知参数。
type appServerTurnCompletedParams struct {
	// ThreadID 标识 completed 事件所属 thread。
	ThreadID string `json:"threadId"`
	// Turn 包含 turn 最终状态和消息列表。
	Turn appServerTurn `json:"turn"`
}

// appServerTurn 表示 codex app-server 返回的 turn 摘要。
type appServerTurn struct {
	// ID 是 turn 标识。
	ID string `json:"id"`
	// Status 是 turn 最终状态，例如 completed 或 failed。
	Status string `json:"status"`
	// Error 是 turn 失败时的错误信息。
	Error *appServerTurnError `json:"error"`
	// Items 是 turn 生成的消息条目。
	Items []appServerItem `json:"items"`
}

// appServerTurnError 表示 turn 级错误。
type appServerTurnError struct {
	// Message 是可返回给 HTTP 调用方的错误文案。
	Message string `json:"message"`
}

// appServerItem 表示 codex app-server turn 中的一条消息项。
type appServerItem struct {
	// Type 是 item 类型；当前只读取 agentMessage。
	Type string `json:"type"`
	// Text 是 agentMessage 的正文。
	Text string `json:"text"`
}

// runCodexAppServerStream 通过 codex app-server JSON-RPC 协议执行一次流式 turn。
func runCodexAppServerStream(ctx context.Context, prompt string, timeout time.Duration, spec execCommandSpec, writer StreamWriter, cwd string, workspaceRoot string, appOptions CodexAppServerOptions) codexAppServerRun {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := newChildCommand(spec)

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

// handleCodexAppServerLine 处理 app-server 单行 JSON-RPC 消息并驱动 initialize/thread/start/turn/start 流程。
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

// appServerDeltaTracker 按 item/turn/thread 记录已见文本，用于把累计文本转成真正增量。
type appServerDeltaTracker struct {
	// seen 保存每个消息键最后一次看到的完整文本。
	seen map[string]string
}

// newAppServerDeltaTracker 创建空的 app-server delta 去重器。
func newAppServerDeltaTracker() *appServerDeltaTracker {
	return &appServerDeltaTracker{seen: map[string]string{}}
}

// Normalize 将 app-server delta 归一化成未发送过的文本片段。
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

// finishCodexAppServerRun 收集 app-server 进程退出状态、stdout/stderr 和等待错误。
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

	exitCode, resolveErr := resolveExitCode(waitErr, cmd)
	result.ExitCode = exitCode
	if resolveErr != nil && result.Err == nil {
		result.Err = resolveErr
	}
	return result
}

// jsonNumberID 把 JSON-RPC ID 解析为 int，兼容数字和字符串 ID。
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

// lastAgentMessageText 返回 turn items 中最后一条 agentMessage 文本。
func lastAgentMessageText(items []appServerItem) string {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == "agentMessage" {
			return items[i].Text
		}
	}
	return ""
}

// normalizedCodexAppServerOptions 填充 codex app-server 运行策略默认值。
func normalizedCodexAppServerOptions(options CodexAppServerOptions) CodexAppServerOptions {
	if strings.TrimSpace(options.ApprovalPolicy) == "" {
		options.ApprovalPolicy = "never"
	}
	if strings.TrimSpace(options.Sandbox) == "" {
		options.Sandbox = "workspace-write"
	}
	return options
}

// codexAppServerEphemeral 返回 ephemeral 配置；未配置时默认启用临时 thread。
func codexAppServerEphemeral(options CodexAppServerOptions) bool {
	if options.Ephemeral == nil {
		return true
	}
	return *options.Ephemeral
}

package agenthttp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// sendError 根据错误类型选择 HTTP 状态码并返回统一 JSON 错误体。
func (s *Server) sendError(response http.ResponseWriter, err error) {
	var requestErr *RequestError
	if errors.As(err, &requestErr) {
		sendJSON(response, requestErr.StatusCode, map[string]any{"ok": false, "error": requestErr.Message})
		return
	}
	sendJSON(response, http.StatusInternalServerError, map[string]any{"ok": false, "error": errorMessage(err)})
}

// readJSONBody 保持 Node 参考实现的行为：空请求体会变成空请求对象，
// prompt 等业务校验交给 runner 处理。
func (s *Server) readJSONBody(request *http.Request) (RunRequest, error) {
	defer request.Body.Close()

	decoder := json.NewDecoder(http.MaxBytesReader(nil, request.Body, s.maxBodyBytes))
	var body RunRequest
	if err := decoder.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return RunRequest{}, nil
		}
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return RunRequest{}, NewRequestError("request body too large", http.StatusRequestEntityTooLarge)
		}
		return RunRequest{}, NewRequestError("invalid JSON body", http.StatusBadRequest)
	}
	return body, nil
}

// formatRunResult 默认隐藏原始 stdout/stderr；只有调用方显式传 debug=1 时才返回。
func formatRunResult(result RunResult, includeDebug bool) map[string]any {
	response := map[string]any{"ok": result.OK}
	if result.SessionID != "" {
		response["sessionId"] = result.SessionID
	}
	if result.Error != "" {
		response["error"] = result.Error
	}
	if result.ExitCode != nil {
		response["exitCode"] = *result.ExitCode
	}
	if result.Output != "" || !result.OK {
		response["output"] = result.Output
	}
	if result.TimedOut {
		response["timedOut"] = true
	}
	if includeDebug {
		response["debug"] = map[string]any{
			"stdout": result.Stdout,
			"stderr": result.Stderr,
		}
	}
	return response
}

// sendStreamResultIfDebug 仅在 debug 模式下发送包含原始 stdout/stderr 的 result 事件。
func sendStreamResultIfDebug(stream *sseStream, result RunResult, includeDebug bool) {
	if !includeDebug {
		return
	}
	stream.send("result", formatRunResult(result, true))
}

// formatStreamDone 构建 SSE done 事件的载荷，不含 debug 信息以减少事件体积。
func formatStreamDone(result RunResult) map[string]any {
	response := map[string]any{"ok": result.OK}
	if result.SessionID != "" {
		response["sessionId"] = result.SessionID
	}
	if result.Error != "" {
		response["error"] = result.Error
	}
	if result.ExitCode != nil {
		response["exitCode"] = *result.ExitCode
	}
	if result.TimedOut {
		response["timedOut"] = true
	}
	return response
}

// sendJSON 写出统一的 JSON 响应头和响应体。
func sendJSON(response http.ResponseWriter, statusCode int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		statusCode = http.StatusInternalServerError
		payload = []byte(`{"ok":false,"error":"internal server error"}`)
	}

	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	response.WriteHeader(statusCode)
	_, _ = response.Write(payload)
}

// sseStream 封装 Server-Sent Events 响应流，把 runner 输出转成 SSE 事件。
type sseStream struct {
	// response 是 HTTP 响应写入器。
	response http.ResponseWriter
	// flusher 用于即时刷新缓冲区，确保客户端能尽快收到事件。
	flusher http.Flusher
}

// newSSEStream 初始化 Server-Sent Events 响应头。
// SSE 仍基于 HTTP，适合服务端单向推送 start/delta/done 这类 chat 事件。
func newSSEStream(response http.ResponseWriter) (*sseStream, bool) {
	flusher, ok := response.(http.Flusher)
	if !ok {
		return nil, false
	}

	header := response.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	flusher.Flush()

	return &sseStream{response: response, flusher: flusher}, true
}

// send 写出一个 SSE 事件。data 使用 JSON，方便客户端按事件类型解析结构化载荷。
func (s *sseStream) send(event string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		payload = []byte(`{"ok":false,"error":"internal server error"}`)
	}

	_, _ = s.response.Write([]byte("event: " + event + "\n"))
	for _, line := range strings.Split(string(payload), "\n") {
		_, _ = s.response.Write([]byte("data: " + line + "\n"))
	}
	_, _ = s.response.Write([]byte("\n"))
	s.flusher.Flush()
}

// WriteDelta 实现 StreamWriter，把 runner stdout 片段转成 SSE delta 事件。
func (s *sseStream) WriteDelta(delta string) error {
	s.send("delta", map[string]any{"delta": delta})
	return nil
}

// errorMessage 返回非空错误文案，避免给调用方返回空字符串。
func errorMessage(err error) string {
	if err == nil {
		return "internal server error"
	}
	if err.Error() == "" {
		return "internal server error"
	}
	return err.Error()
}

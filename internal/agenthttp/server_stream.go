package agenthttp

import (
	"net/http"
)

// handleRunStream 处理无持久化历史的 SSE 运行接口。
// StreamRunner 会把 CLI stdout 片段推成 delta；默认只用 done 返回最终状态。
func (s *Server) handleRunStream(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/codex/stream" {
		markDeprecatedEndpoint(response, "/runs/stream")
	}
	if request.Method != http.MethodPost {
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}

	body, err := s.readJSONBody(request)
	if err != nil {
		s.sendError(response, err)
		return
	}

	runnerPath := "/runs"
	if request.URL.Path == "/codex/stream" {
		runnerPath = "/codex"
	}

	runner, err := s.selectStreamRunner(runnerPath, body)
	if err != nil {
		s.sendError(response, err)
		return
	}

	stream, ok := newSSEStream(response)
	if !ok {
		s.sendError(response, NewRequestError("streaming is not supported", http.StatusInternalServerError))
		return
	}
	if err := stream.send("start", map[string]any{"ok": true}); err != nil {
		return
	}

	result, err := runner(request.Context(), body, stream)
	if err != nil {
		if err := stream.send("error", map[string]any{"ok": false, "error": errorMessage(err)}); err != nil {
			return
		}
		_ = stream.send("done", map[string]any{"ok": false})
		return
	}

	if err := sendStreamResultIfDebug(stream, result, request.URL.Query().Get("debug") == "1"); err != nil {
		return
	}
	_ = stream.send("done", formatStreamDone(result))
}

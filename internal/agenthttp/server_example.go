package agenthttp

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const sessionStreamExamplePath = "examples/session-stream/index.html"

// handleSessionStreamExample serves the local console for exercising
// POST /sessions/{sessionId}/runs/stream from the same origin as the API.
func (s *Server) handleSessionStreamExample(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	if request.URL.Path == "/examples/session-stream/" {
		http.Redirect(response, request, "/examples/session-stream", http.StatusMovedPermanently)
		return
	}
	if request.URL.Path != "/examples/session-stream" {
		handleNotFound(response, request)
		return
	}

	payload, err := readSessionStreamExample()
	if err != nil {
		sendJSON(response, http.StatusInternalServerError, map[string]any{"ok": false, "error": errorMessage(err)})
		return
	}

	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-cache")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(payload)
}

func readSessionStreamExample() ([]byte, error) {
	paths := []string{sessionStreamExamplePath}
	if _, file, _, ok := runtime.Caller(0); ok {
		paths = append(paths, filepath.Join(filepath.Dir(file), "..", "..", sessionStreamExamplePath))
	}

	var errors []string
	for _, path := range paths {
		payload, err := os.ReadFile(path)
		if err == nil {
			return payload, nil
		}
		errors = append(errors, path+": "+err.Error())
	}
	return nil, fmt.Errorf("session stream example not found: %s", strings.Join(errors, "; "))
}

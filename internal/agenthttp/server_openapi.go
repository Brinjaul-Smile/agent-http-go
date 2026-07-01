package agenthttp

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const openAPIPath = "docs/openapi.yaml"

// handleOpenAPI serves the OpenAPI contract used by the local Swagger page.
func (s *Server) handleOpenAPI(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	if request.URL.Path != "/openapi.yaml" {
		handleNotFound(response, request)
		return
	}

	payload, err := readOpenAPI()
	if err != nil {
		sendJSON(response, http.StatusInternalServerError, map[string]any{"ok": false, "error": errorMessage(err)})
		return
	}

	response.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	response.Header().Set("Cache-Control", "no-cache")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(payload)
}

// handleSwagger serves a small same-origin Swagger UI wrapper for /openapi.yaml.
func (s *Server) handleSwagger(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	if request.URL.Path == "/swagger/" {
		http.Redirect(response, request, "/swagger", http.StatusMovedPermanently)
		return
	}
	if request.URL.Path != "/swagger" {
		handleNotFound(response, request)
		return
	}

	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-cache")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte(swaggerHTML))
}

func readOpenAPI() ([]byte, error) {
	paths := []string{openAPIPath}
	if _, file, _, ok := runtime.Caller(0); ok {
		paths = append(paths, filepath.Join(filepath.Dir(file), "..", "..", openAPIPath))
	}

	var errors []string
	for _, path := range paths {
		payload, err := os.ReadFile(path)
		if err == nil {
			return payload, nil
		}
		errors = append(errors, path+": "+err.Error())
	}
	return nil, fmt.Errorf("openapi document not found: %s", strings.Join(errors, "; "))
}

const swaggerHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Agent HTTP Go API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>
    html,
    body {
      margin: 0;
      min-height: 100%;
      background: #f7f7f7;
    }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.addEventListener("load", function () {
      SwaggerUIBundle({
        url: "/openapi.yaml",
        dom_id: "#swagger-ui",
        deepLinking: true,
        presets: [SwaggerUIBundle.presets.apis],
        layout: "BaseLayout"
      });
    });
  </script>
</body>
</html>
`

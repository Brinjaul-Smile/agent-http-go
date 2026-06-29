package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/Brinjaul-Smile/agent-http-go/internal/agenthttp"
)

func main() {
	// 先加载配置文件，再允许 HOST/PORT 环境变量覆盖监听地址。
	config, err := LoadConfig(ConfigOptions{
		Path: os.Getenv("CONFIG_FILE"),
	})
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	server := agenthttp.NewServer(agenthttp.ServerOptions{
		Env: os.Environ(),
	})

	// 使用标准库 slog 记录启动和异常退出，避免混用 fmt/log 输出。
	addr := config.Host + ":" + config.Port
	slog.Info("agent HTTP server listening", "url", "http://"+addr)
	if err := http.ListenAndServe(addr, server); err != nil {
		slog.Error("agent HTTP server stopped", "error", err)
		os.Exit(1)
	}
}

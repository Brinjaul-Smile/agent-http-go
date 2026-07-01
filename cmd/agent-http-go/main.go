package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Brinjaul-Smile/agent-http-go/internal/agenthttp"
)

const (
	// defaultShutdownTimeout 是优雅关闭 HTTP 服务的默认等待时间。
	defaultShutdownTimeout = 10 * time.Second
	// defaultReadHeaderTimeout 限制读取 HTTP 请求头的时间，防止慢连接长期占用。
	defaultReadHeaderTimeout = 5 * time.Second
	// defaultReadTimeout 限制读取完整请求体的时间；SSE 响应不设置 WriteTimeout。
	defaultReadTimeout = 30 * time.Second
	// defaultIdleTimeout 限制 keep-alive 空闲连接保留时间。
	defaultIdleTimeout = 120 * time.Second
)

// main 是 agent-http-go 服务入口，依次完成配置加载、日志初始化、会话存储创建和 HTTP 服务启动。
func main() {
	// 先加载配置文件，再允许 HOST/PORT 环境变量覆盖监听地址。
	config, err := LoadConfig(ConfigOptions{
		Path: os.Getenv("CONFIG_FILE"),
	})
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logger := newLogger(config)
	slog.SetDefault(logger)

	if err := serve(context.Background(), config, logger); err != nil {
		slog.Error("agent HTTP server stopped", "error", err)
		os.Exit(1)
	}
}

// serve 创建 session 存储、构建 HTTP handler 并启动 HTTP 服务。
func serve(ctx context.Context, config Config, logger *slog.Logger) error {
	sessionStore, err := newSessionStore(config)
	if err != nil {
		return err
	}
	if sessionStore != nil {
		defer func() {
			if err := sessionStore.Close(); err != nil {
				logger.Error("failed to close session store", "error", err)
			}
		}()
	}
	codexAppServerOptions := agenthttp.CodexAppServerOptions{
		ApprovalPolicy: config.CodexApprovalPolicy,
		Sandbox:        config.CodexSandbox,
		Ephemeral:      &config.CodexEphemeral,
	}

	handler := agenthttp.NewServer(agenthttp.ServerOptions{
		WorkspaceRoot:  config.WorkspaceRoot,
		Env:            os.Environ(),
		Timeout:        config.RunnerTimeout,
		CodexCommand:   config.CodexCommand,
		ClaudeCommand:  config.ClaudeCommand,
		CodexAppServer: codexAppServerOptions,
		MaxBodyBytes:   config.MaxBodyBytes,
		LogRoutes:      config.LogRoutes,
		EnableSwagger:  config.SwaggerEnabled,
		EnableExamples: config.ExamplesEnabled,
		Logger:         logger,
		SessionStore:   sessionStore,
		SessionOptions: agenthttp.SessionRunOptions{MaxTurns: config.SessionMaxTurns, MaxHistoryBytes: config.SessionMaxHistoryBytes},
	})

	// 使用标准库 slog 记录启动和异常退出，避免混用 fmt/log 输出。
	addr := config.Host + ":" + config.Port
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: config.ReadHeaderTimeout,
		ReadTimeout:       config.ReadTimeout,
		IdleTimeout:       config.IdleTimeout,
	}
	return runHTTPServer(ctx, server, logger, config.ShutdownTimeout)
}

// closeableSessionStore 是可关闭的 SessionStore，允许启动阶段选择不同存储实现。
type closeableSessionStore interface {
	agenthttp.SessionStore
	Close() error
}

// newSessionStore 根据配置创建会话存储实例；未启用时返回 nil。
func newSessionStore(config Config) (closeableSessionStore, error) {
	if !config.SessionEnabled {
		return nil, nil
	}
	switch config.SessionDriver {
	case "sqlite":
		return agenthttp.OpenSQLiteSessionStore(config.SessionSQLitePath)
	case "mysql":
		return agenthttp.OpenMySQLSessionStore(config.SessionMySQLDSN)
	default:
		return nil, fmt.Errorf("unsupported session driver: %s", config.SessionDriver)
	}
}

// runHTTPServer 启动 HTTP 服务并等待中断信号后优雅关闭。
func runHTTPServer(ctx context.Context, server *http.Server, logger *slog.Logger, shutdownTimeout time.Duration) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	logger.Info("agent HTTP server listening", "url", "http://"+server.Addr)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		stop()
	}

	logger.Info("agent HTTP server shutting down", "timeout", shutdownTimeout)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		if closeErr := server.Close(); closeErr != nil {
			return fmt.Errorf("graceful shutdown failed: %w; forced close failed: %v", err, closeErr)
		}
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}

	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	logger.Info("agent HTTP server stopped")
	return nil
}

// newLogger 根据配置创建 slog logger。
func newLogger(config Config) *slog.Logger {
	options := &slog.HandlerOptions{
		Level: config.LogLevel,
	}
	if config.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, options))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, options))
}

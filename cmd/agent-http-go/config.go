package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath             = "config.yaml"
	defaultHost                   = "127.0.0.1"
	defaultPort                   = "8787"
	defaultTimeout                = 10 * time.Minute
	defaultMaxBody                = int64(1024 * 1024)
	defaultWorkspace              = "."
	defaultLogFormat              = "text"
	defaultSessionDriver          = "sqlite"
	defaultSessionSQLitePath      = "./data/agent-http.db"
	defaultSessionMaxTurns        = 20
	defaultSessionMaxHistoryBytes = 64 * 1024
	defaultCodexCommand           = "codex"
	defaultClaudeCommand          = "claude"
	defaultCodexApprovalPolicy    = "never"
	defaultCodexSandbox           = "workspace-write"
	defaultCodexEphemeral         = true
)

// Config 表示命令入口运行 HTTP 服务所需的配置。
type Config struct {
	// Host 和 Port 控制 HTTP 服务监听地址；部署时可被 HOST/PORT 环境变量覆盖。
	Host string
	Port string
	// ShutdownTimeout 控制收到中断信号后等待 HTTP 服务优雅关闭的最长时间。
	ShutdownTimeout time.Duration
	// LogRoutes 控制启动时是否把注册路由写入日志。
	LogRoutes bool
	// MaxBodyBytes 限制 JSON 请求体大小，避免过大的 prompt 或调试载荷占用内存。
	MaxBodyBytes int64
	// RunnerTimeout 控制单次 agent CLI 子进程允许运行的最长时间。
	RunnerTimeout time.Duration
	// CodexCommand 和 ClaudeCommand 控制服务查找或执行的 agent CLI 命令。
	CodexCommand  string
	ClaudeCommand string
	// CodexApprovalPolicy、CodexSandbox 和 CodexEphemeral 透传给 codex app-server 的 thread/start。
	CodexApprovalPolicy string
	CodexSandbox        string
	CodexEphemeral      bool
	// WorkspaceRoot 限定请求 cwd 的边界，避免 agent 在工作区外执行。
	WorkspaceRoot string
	// LogLevel 和 LogFormat 控制 slog 输出级别和 text/json 格式。
	LogLevel  slog.Level
	LogFormat string
	// SessionEnabled 控制是否启用持久会话；SessionDriver 当前只支持 sqlite。
	SessionEnabled bool
	SessionDriver  string
	// SessionSQLitePath 是 sqlite 会话库文件路径。
	SessionSQLitePath string
	// SessionMaxTurns 和 SessionMaxHistoryBytes 控制注入 runner prompt 的历史窗口。
	SessionMaxTurns        int
	SessionMaxHistoryBytes int
}

// ConfigOptions 控制配置文件路径和环境变量来源。
// Env 可注入，测试时不用修改真实进程环境也能验证环境变量覆盖逻辑。
type ConfigOptions struct {
	// Path 为空时使用默认配置文件 config.yaml；文件不存在时继续使用默认配置。
	Path string
	// Env 为 nil 时读取真实进程环境；非 nil 时只使用传入的键值覆盖。
	Env map[string]string
}

// configFile 对应 YAML 文件的顶层结构。
type configFile struct {
	Server    serverConfig    `yaml:"server"`
	Runner    runnerConfig    `yaml:"runner"`
	Workspace workspaceConfig `yaml:"workspace"`
	Log       logConfig       `yaml:"log"`
	Session   sessionConfig   `yaml:"session"`
}

// serverConfig 对应 YAML 中的 server 配置段。
type serverConfig struct {
	// host/port 控制 HTTP 服务监听地址。
	Host string `yaml:"host"`
	Port string `yaml:"port"`
	// shutdownTimeout 使用 Go duration 格式，例如 10s、1m。
	ShutdownTimeout string `yaml:"shutdownTimeout"`
	// logRoutes 为 true 时启动阶段会输出所有注册路由。
	LogRoutes bool `yaml:"logRoutes"`
	// maxBodySize 支持 B、KiB、MiB、GiB 或裸字节数。
	MaxBodySize string `yaml:"maxBodySize"`
}

// runnerConfig 对应 YAML 中的 runner 配置段。
type runnerConfig struct {
	// timeout 控制一次 agent CLI 调用的整体超时。
	Timeout string            `yaml:"timeout"`
	Codex   codexRunnerConfig `yaml:"codex"`
	Claude  commandConfig     `yaml:"claude"`
}

// codexRunnerConfig 对应 YAML 中的 runner.codex 配置段。
type codexRunnerConfig struct {
	// command 可以是 PATH 中的命令名，也可以是可执行文件绝对路径。
	Command string `yaml:"command"`
	// approvalPolicy、sandbox 和 ephemeral 原样传给 codex app-server 的 thread/start。
	ApprovalPolicy string `yaml:"approvalPolicy"`
	Sandbox        string `yaml:"sandbox"`
	// Ephemeral 使用指针区分“未配置”和显式配置 false。
	Ephemeral *bool `yaml:"ephemeral"`
}

// commandConfig 表示只需要配置 CLI 命令名的 agent 配置段。
type commandConfig struct {
	// command 可以是 PATH 中的命令名，也可以是可执行文件绝对路径。
	Command string `yaml:"command"`
}

// workspaceConfig 对应 YAML 中的 workspace 配置段。
type workspaceConfig struct {
	// root 是服务允许 agent 使用的工作区根目录。
	Root string `yaml:"root"`
}

// logConfig 对应 YAML 中的 log 配置段。
type logConfig struct {
	// level 支持 debug、info、warn、error。
	Level string `yaml:"level"`
	// format 支持 text 和 json。
	Format string `yaml:"format"`
}

// sessionConfig 对应 YAML 中的 session 配置段。
type sessionConfig struct {
	// Enabled 使用指针区分“未配置”和显式关闭 false。
	Enabled *bool `yaml:"enabled"`
	// Driver 当前只支持 sqlite。
	Driver string `yaml:"driver"`
	// MaxTurns 控制最多取多少轮成功历史拼进下一次 prompt。
	MaxTurns int `yaml:"maxTurns"`
	// MaxHistorySize 是推荐字段；MaxHistoryBytes 保留兼容旧配置。
	MaxHistorySize  string              `yaml:"maxHistorySize"`
	MaxHistoryBytes string              `yaml:"maxHistoryBytes"`
	SQLite          sessionSQLiteConfig `yaml:"sqlite"`
}

// sessionSQLiteConfig 对应 YAML 中的 session.sqlite 配置段。
type sessionSQLiteConfig struct {
	// path 是 sqlite 数据库文件路径。
	Path string `yaml:"path"`
}

// LoadConfig 按默认值、YAML 文件、环境变量的顺序加载运行配置。
func LoadConfig(options ConfigOptions) (Config, error) {
	config := Config{
		Host:                   defaultHost,
		Port:                   defaultPort,
		ShutdownTimeout:        defaultShutdownTimeout,
		MaxBodyBytes:           defaultMaxBody,
		RunnerTimeout:          defaultTimeout,
		CodexCommand:           defaultCodexCommand,
		ClaudeCommand:          defaultClaudeCommand,
		CodexApprovalPolicy:    defaultCodexApprovalPolicy,
		CodexSandbox:           defaultCodexSandbox,
		CodexEphemeral:         defaultCodexEphemeral,
		WorkspaceRoot:          defaultWorkspace,
		LogLevel:               slog.LevelInfo,
		LogFormat:              defaultLogFormat,
		SessionEnabled:         true,
		SessionDriver:          defaultSessionDriver,
		SessionSQLitePath:      defaultSessionSQLitePath,
		SessionMaxTurns:        defaultSessionMaxTurns,
		SessionMaxHistoryBytes: defaultSessionMaxHistoryBytes,
	}

	path := options.Path
	if path == "" {
		path = defaultConfigPath
	}

	fileConfig, err := readConfigFile(path)
	if err != nil {
		return Config{}, err
	}
	if fileConfig.Server.Host != "" {
		config.Host = fileConfig.Server.Host
	}
	if fileConfig.Server.Port != "" {
		config.Port = fileConfig.Server.Port
	}
	if fileConfig.Server.ShutdownTimeout != "" {
		timeout, err := time.ParseDuration(fileConfig.Server.ShutdownTimeout)
		if err != nil {
			return Config{}, err
		}
		config.ShutdownTimeout = timeout
	}
	config.LogRoutes = fileConfig.Server.LogRoutes
	if fileConfig.Server.MaxBodySize != "" {
		maxBodyBytes, err := parseByteSize(fileConfig.Server.MaxBodySize)
		if err != nil {
			return Config{}, err
		}
		config.MaxBodyBytes = maxBodyBytes
	}
	if fileConfig.Runner.Timeout != "" {
		timeout, err := time.ParseDuration(fileConfig.Runner.Timeout)
		if err != nil {
			return Config{}, err
		}
		config.RunnerTimeout = timeout
	}
	if fileConfig.Runner.Codex.Command != "" {
		config.CodexCommand = fileConfig.Runner.Codex.Command
	}
	if fileConfig.Runner.Codex.ApprovalPolicy != "" {
		config.CodexApprovalPolicy = fileConfig.Runner.Codex.ApprovalPolicy
	}
	if fileConfig.Runner.Codex.Sandbox != "" {
		config.CodexSandbox = fileConfig.Runner.Codex.Sandbox
	}
	if fileConfig.Runner.Codex.Ephemeral != nil {
		config.CodexEphemeral = *fileConfig.Runner.Codex.Ephemeral
	}
	if fileConfig.Runner.Claude.Command != "" {
		config.ClaudeCommand = fileConfig.Runner.Claude.Command
	}
	if fileConfig.Workspace.Root != "" {
		config.WorkspaceRoot = fileConfig.Workspace.Root
	}
	if fileConfig.Log.Level != "" {
		level, err := parseLogLevel(fileConfig.Log.Level)
		if err != nil {
			return Config{}, err
		}
		config.LogLevel = level
	}
	if fileConfig.Log.Format != "" {
		format, err := parseLogFormat(fileConfig.Log.Format)
		if err != nil {
			return Config{}, err
		}
		config.LogFormat = format
	}
	if fileConfig.Session.Enabled != nil {
		config.SessionEnabled = *fileConfig.Session.Enabled
	}
	if fileConfig.Session.Driver != "" {
		driver, err := parseSessionDriver(fileConfig.Session.Driver)
		if err != nil {
			return Config{}, err
		}
		config.SessionDriver = driver
	}
	if fileConfig.Session.SQLite.Path != "" {
		config.SessionSQLitePath = fileConfig.Session.SQLite.Path
	}
	if fileConfig.Session.MaxTurns != 0 {
		if fileConfig.Session.MaxTurns <= 0 {
			return Config{}, fmt.Errorf("session maxTurns must be positive")
		}
		config.SessionMaxTurns = fileConfig.Session.MaxTurns
	}
	maxHistorySize := fileConfig.Session.MaxHistorySize
	if maxHistorySize == "" {
		maxHistorySize = fileConfig.Session.MaxHistoryBytes
	}
	if maxHistorySize != "" {
		maxHistoryBytes, err := parseByteSize(maxHistorySize)
		if err != nil {
			return Config{}, err
		}
		config.SessionMaxHistoryBytes = int(maxHistoryBytes)
	}

	// 环境变量放在最后覆盖，部署时可以不改配置文件就临时调整监听地址。
	env := options.Env
	if env == nil {
		env = environMap()
	}
	if value := env["HOST"]; value != "" {
		config.Host = value
	}
	if value := env["PORT"]; value != "" {
		config.Port = value
	}

	return config, nil
}

// parseByteSize 解析配置中的字节大小，支持 B、KiB、MiB、GiB。
func parseByteSize(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("byte size must not be empty")
	}

	units := []struct {
		suffix string
		scale  int64
	}{
		{suffix: "GiB", scale: 1024 * 1024 * 1024},
		{suffix: "MiB", scale: 1024 * 1024},
		{suffix: "KiB", scale: 1024},
		{suffix: "B", scale: 1},
	}

	for _, unit := range units {
		if strings.HasSuffix(trimmed, unit.suffix) {
			number := strings.TrimSpace(strings.TrimSuffix(trimmed, unit.suffix))
			size, err := strconv.ParseInt(number, 10, 64)
			if err != nil || size <= 0 {
				return 0, fmt.Errorf("invalid byte size: %s", value)
			}
			return size * unit.scale, nil
		}
	}

	size, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || size <= 0 {
		return 0, fmt.Errorf("invalid byte size: %s", value)
	}
	return size, nil
}

// parseLogLevel 解析 slog 支持的日志级别。
func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level: %s", value)
	}
}

// parseLogFormat 校验日志输出格式。
func parseLogFormat(value string) (string, error) {
	format := strings.ToLower(strings.TrimSpace(value))
	switch format {
	case "text", "json":
		return format, nil
	default:
		return "", fmt.Errorf("invalid log format: %s", value)
	}
}

// parseSessionDriver 校验持久会话存储驱动；当前实现 sqlite，接口保留 RDS 扩展空间。
func parseSessionDriver(value string) (string, error) {
	driver := strings.ToLower(strings.TrimSpace(value))
	switch driver {
	case "sqlite":
		return driver, nil
	default:
		return "", fmt.Errorf("unsupported session driver: %s", value)
	}
}

// readConfigFile 读取 YAML 配置文件；文件不存在时使用默认配置继续启动。
func readConfigFile(path string) (configFile, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return configFile{}, nil
		}
		return configFile{}, err
	}

	var config configFile
	if err := yaml.Unmarshal(payload, &config); err != nil {
		return configFile{}, err
	}
	return config, nil
}

// environMap 将当前进程环境变量转换成便于覆盖配置的 map。
func environMap() map[string]string {
	env := make(map[string]string)
	for _, item := range os.Environ() {
		for i, char := range item {
			if char == '=' {
				env[item[:i]] = item[i+1:]
				break
			}
		}
	}
	return env
}

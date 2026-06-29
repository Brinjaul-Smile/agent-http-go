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
	defaultConfigPath = "config.yaml"
	defaultHost       = "127.0.0.1"
	defaultPort       = "8787"
	defaultTimeout    = 10 * time.Minute
	defaultMaxBody    = int64(1024 * 1024)
	defaultWorkspace  = "."
	defaultLogFormat  = "text"
)

// Config 表示命令入口运行 HTTP 服务所需的配置。
type Config struct {
	Host          string
	Port          string
	LogRoutes     bool
	MaxBodyBytes  int64
	RunnerTimeout time.Duration
	WorkspaceRoot string
	LogLevel      slog.Level
	LogFormat     string
}

// ConfigOptions 控制配置文件路径和环境变量来源。
// Env 可注入，测试时不用修改真实进程环境也能验证环境变量覆盖逻辑。
type ConfigOptions struct {
	Path string
	Env  map[string]string
}

// configFile 对应 YAML 文件的顶层结构。
type configFile struct {
	Server    serverConfig    `yaml:"server"`
	Runner    runnerConfig    `yaml:"runner"`
	Workspace workspaceConfig `yaml:"workspace"`
	Log       logConfig       `yaml:"log"`
}

// serverConfig 对应 YAML 中的 server 配置段。
type serverConfig struct {
	Host        string `yaml:"host"`
	Port        string `yaml:"port"`
	LogRoutes   bool   `yaml:"logRoutes"`
	MaxBodySize string `yaml:"maxBodySize"`
}

// runnerConfig 对应 YAML 中的 runner 配置段。
type runnerConfig struct {
	Timeout string `yaml:"timeout"`
}

// workspaceConfig 对应 YAML 中的 workspace 配置段。
type workspaceConfig struct {
	Root string `yaml:"root"`
}

// logConfig 对应 YAML 中的 log 配置段。
type logConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// LoadConfig 按默认值、YAML 文件、环境变量的顺序加载运行配置。
func LoadConfig(options ConfigOptions) (Config, error) {
	config := Config{
		Host:          defaultHost,
		Port:          defaultPort,
		MaxBodyBytes:  defaultMaxBody,
		RunnerTimeout: defaultTimeout,
		WorkspaceRoot: defaultWorkspace,
		LogLevel:      slog.LevelInfo,
		LogFormat:     defaultLogFormat,
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

package agenthttp

import (
	"os"
	"path/filepath"
	"strings"
)

// AgentConfig 描述 /agents 接口需要检查的一个 agent CLI 命令。
type AgentConfig struct {
	Name      string
	Command   string
	Supported bool
}

// AgentStatus 表示一个已知 agent 的可用性检查结果，会直接序列化给 HTTP 调用方。
type AgentStatus struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Available bool   `json:"available"`
	Supported bool   `json:"supported"`
	Error     string `json:"error,omitempty"`
}

// DefaultKnownAgents 返回 /agents 默认检查的 agent 命令列表。
func DefaultKnownAgents() []AgentConfig {
	return []AgentConfig{
		{Name: "codex", Command: "codex", Supported: true},
		{Name: "claude", Command: "claude", Supported: true},
	}
}

// GetAgentAvailability 只检查 PATH 中是否存在命令，不真正调用模型或 CLI。
// 这样 /agents 接口开销小，并且没有副作用。
func GetAgentAvailability(agents []AgentConfig, env []string) ([]AgentStatus, error) {
	statuses := make([]AgentStatus, 0, len(agents))
	for _, agent := range agents {
		path, err := FindExecutable(agent.Command, env)
		if err != nil {
			return nil, err
		}

		status := AgentStatus{
			Name:      agent.Name,
			Command:   agent.Command,
			Available: path != "",
			Supported: agent.Supported,
		}
		if path == "" {
			status.Error = agent.Command + " CLI not found in PATH"
		}

		statuses = append(statuses, status)
	}

	return statuses, nil
}

// FindExecutable 在传入的环境变量 PATH 中查找可执行命令。
func FindExecutable(command string, env []string) (string, error) {
	if filepath.IsAbs(command) {
		if isExecutable, err := isExecutableFile(command); err != nil || !isExecutable {
			return "", err
		}
		return command, nil
	}

	for _, directory := range pathDirectories(env) {
		candidate := filepath.Join(directory, command)
		isExecutable, err := isExecutableFile(candidate)
		if err != nil {
			return "", err
		}
		if isExecutable {
			return candidate, nil
		}
	}

	return "", nil
}

// isExecutableFile 检查路径是否指向可执行文件；文件不存在时返回 (false, nil) 而非错误。
func isExecutableFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return !info.IsDir() && info.Mode()&0o111 != 0, nil
}

// pathDirectories 从环境变量中解析 PATH 目录列表。
func pathDirectories(env []string) []string {
	pathValue := ""
	for _, item := range env {
		if strings.HasPrefix(item, "PATH=") {
			pathValue = strings.TrimPrefix(item, "PATH=")
			break
		}
	}
	if pathValue == "" {
		return nil
	}

	parts := strings.Split(pathValue, string(os.PathListSeparator))
	directories := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			directories = append(directories, part)
		}
	}
	return directories
}

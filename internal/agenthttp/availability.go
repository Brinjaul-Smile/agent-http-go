package agenthttp

import (
	"os"
	"path/filepath"
	"strings"
)

type AgentConfig struct {
	Name      string
	Command   string
	Supported bool
}

type AgentStatus struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Available bool   `json:"available"`
	Supported bool   `json:"supported"`
	Error     string `json:"error,omitempty"`
}

func DefaultKnownAgents() []AgentConfig {
	return []AgentConfig{
		{Name: "codex", Command: "codex", Supported: true},
		{Name: "claude", Command: "claude", Supported: true},
		{Name: "gemini", Command: "gemini", Supported: false},
		{Name: "opencode", Command: "opencode", Supported: false},
		{Name: "pi", Command: "pi", Supported: false},
		{Name: "cursor-agent", Command: "cursor-agent", Supported: false},
		{Name: "aider", Command: "aider", Supported: false},
		{Name: "amp", Command: "amp", Supported: false},
		{Name: "auggie", Command: "auggie", Supported: false},
		{Name: "goose", Command: "goose", Supported: false},
		{Name: "qwen", Command: "qwen", Supported: false},
	}
}

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

func FindExecutable(command string, env []string) (string, error) {
	for _, directory := range pathDirectories(env) {
		candidate := filepath.Join(directory, command)
		info, err := os.Stat(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		if !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}

	return "", nil
}

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

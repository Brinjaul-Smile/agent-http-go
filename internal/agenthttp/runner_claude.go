package agenthttp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// RunClaude 以 JSON 输出模式执行 Claude Code，并把 result 字段归一化成 RunResult。
func RunClaude(body RunRequest, options RunnerOptions) (RunResult, error) {
	return RunClaudeContext(context.Background(), body, options)
}

// RunClaudeContext 以 JSON 输出模式执行 Claude Code，并把 result 字段归一化成 RunResult。
func RunClaudeContext(ctx context.Context, body RunRequest, options RunnerOptions) (RunResult, error) {
	env := runnerEnv(options.Env)
	workspaceRoot := runnerWorkspaceRoot(options.WorkspaceRoot)
	timeout := runnerTimeout(options.Timeout)

	prompt, err := ValidatePrompt(body)
	if err != nil {
		return RunResult{}, err
	}

	cwd, err := ResolveWorkspaceCwd(body.Cwd, workspaceRoot)
	if err != nil {
		return RunResult{}, err
	}

	path, err := FindExecutable(runnerCommand(options.ClaudeCommand, "claude"), env)
	if err != nil {
		return RunResult{}, err
	}
	if path == "" {
		return RunResult{}, NewRequestError("claude CLI not found in PATH", http.StatusServiceUnavailable)
	}

	childResult := runChild(ctx, prompt, timeout, execCommandSpec{
		Name: path,
		Args: []string{"--bare", "-p", "--output-format", "json"},
		Cwd:  cwd,
		Env:  env,
	})

	if childResult.TimedOut {
		return RunResult{
			OK:       false,
			Error:    "claude execution timed out",
			ExitCode: childResult.ExitCode,
			TimedOut: true,
			Output:   "",
			Stdout:   childResult.Stdout,
			Stderr:   childResult.Stderr,
		}, nil
	}
	if childResult.Err != nil {
		return RunResult{}, childResult.Err
	}
	if childResult.ExitCode == nil || *childResult.ExitCode != 0 {
		code := 0
		if childResult.ExitCode != nil {
			code = *childResult.ExitCode
		}
		return RunResult{
			OK:       false,
			Error:    "claude exited with code " + intString(code),
			ExitCode: childResult.ExitCode,
			Output:   "",
			Stdout:   childResult.Stdout,
			Stderr:   childResult.Stderr,
		}, nil
	}

	output, err := ParseClaudeOutput(childResult.Stdout)
	if err != nil {
		return RunResult{}, err
	}

	return RunResult{
		OK:       true,
		ExitCode: childResult.ExitCode,
		Output:   output,
		Stdout:   childResult.Stdout,
		Stderr:   childResult.Stderr,
	}, nil
}

// RunClaudeStreamContext 以流式方式执行 Claude Code。
// 当前仍使用 Claude 的 JSON 输出模式；如果 CLI 实时写 stdout，则片段会被推给 StreamWriter。
func RunClaudeStreamContext(ctx context.Context, body RunRequest, writer StreamWriter, options RunnerOptions) (RunResult, error) {
	env := runnerEnv(options.Env)
	workspaceRoot := runnerWorkspaceRoot(options.WorkspaceRoot)
	timeout := runnerTimeout(options.Timeout)

	prompt, err := ValidatePrompt(body)
	if err != nil {
		return RunResult{}, err
	}

	cwd, err := ResolveWorkspaceCwd(body.Cwd, workspaceRoot)
	if err != nil {
		return RunResult{}, err
	}

	path, err := FindExecutable(runnerCommand(options.ClaudeCommand, "claude"), env)
	if err != nil {
		return RunResult{}, err
	}
	if path == "" {
		return RunResult{}, NewRequestError("claude CLI not found in PATH", http.StatusServiceUnavailable)
	}

	childResult := runChildStream(ctx, prompt, timeout, execCommandSpec{
		Name: path,
		Args: []string{"--bare", "-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"},
		Cwd:  cwd,
		Env:  env,
	}, newJSONLineStreamWriter(writer, newClaudeJSONLDeltaParser()))

	return claudeRunResult(childResult)
}

// ParseClaudeOutput 从 Claude 的 JSON stdout 中提取 result 字符串。
func ParseClaudeOutput(stdout string) (string, error) {
	var payload struct {
		Result any `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		return "", NewRequestError("claude returned invalid JSON", http.StatusBadGateway)
	}
	if result, ok := payload.Result.(string); ok {
		return result, nil
	}
	return stdout, nil
}

// ParseClaudeStreamOutput 从 Claude stream-json stdout 中提取最终 result 字段。
func ParseClaudeStreamOutput(stdout string) (string, error) {
	lines := strings.Split(stdout, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			continue
		}
		if result, ok := payload["result"].(string); ok {
			return result, nil
		}
	}
	return ParseClaudeOutput(stdout)
}

func claudeRunResult(childResult childResult) (RunResult, error) {
	if childResult.TimedOut {
		return RunResult{
			OK:       false,
			Error:    "claude execution timed out",
			ExitCode: childResult.ExitCode,
			TimedOut: true,
			Output:   "",
			Stdout:   childResult.Stdout,
			Stderr:   childResult.Stderr,
		}, nil
	}
	if childResult.Err != nil {
		return RunResult{}, childResult.Err
	}
	if childResult.ExitCode == nil || *childResult.ExitCode != 0 {
		code := 0
		if childResult.ExitCode != nil {
			code = *childResult.ExitCode
		}
		return RunResult{
			OK:       false,
			Error:    "claude exited with code " + intString(code),
			ExitCode: childResult.ExitCode,
			Output:   "",
			Stdout:   childResult.Stdout,
			Stderr:   childResult.Stderr,
		}, nil
	}

	output, err := ParseClaudeStreamOutput(childResult.Stdout)
	if err != nil {
		return RunResult{}, err
	}

	return RunResult{
		OK:       true,
		ExitCode: childResult.ExitCode,
		Output:   output,
		Stdout:   childResult.Stdout,
		Stderr:   childResult.Stderr,
	}, nil
}

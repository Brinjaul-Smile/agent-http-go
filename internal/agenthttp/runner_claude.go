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
	prep, err := prepareRunner(body, options, options.ClaudeCommand, "claude", "claude")
	if err != nil {
		return RunResult{}, err
	}

	childResult := runChild(ctx, prep.Prompt, prep.Timeout, execCommandSpec{
		Name: prep.Path,
		Args: []string{"--bare", "-p", "--output-format", "json"},
		Cwd:  prep.Cwd,
		Env:  prep.Env,
	})

	result, err := buildRunResult(childResult, "claude", "")
	if err != nil {
		return RunResult{}, err
	}
	if !result.OK {
		return result, nil
	}

	output, err := ParseClaudeOutput(childResult.Stdout)
	if err != nil {
		return RunResult{}, err
	}

	result.Output = output
	return result, nil
}

// RunClaudeStreamContext 以流式方式执行 Claude Code。
// 当前仍使用 Claude 的 JSON 输出模式；如果 CLI 实时写 stdout，则片段会被推给 StreamWriter。
func RunClaudeStreamContext(ctx context.Context, body RunRequest, writer StreamWriter, options RunnerOptions) (RunResult, error) {
	prep, err := prepareRunner(body, options, options.ClaudeCommand, "claude", "claude")
	if err != nil {
		return RunResult{}, err
	}

	childResult := runChildStream(ctx, prep.Prompt, prep.Timeout, execCommandSpec{
		Name: prep.Path,
		Args: []string{"--bare", "-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages"},
		Cwd:  prep.Cwd,
		Env:  prep.Env,
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

// claudeRunResult 把 Claude stream-json 子进程结果转换成统一 RunResult。
func claudeRunResult(child childResult) (RunResult, error) {
	result, err := buildRunResult(child, "claude", "")
	if err != nil {
		return RunResult{}, err
	}
	if !result.OK {
		return result, nil
	}

	output, err := ParseClaudeStreamOutput(child.Stdout)
	if err != nil {
		return RunResult{}, err
	}

	result.Output = output
	return result, nil
}

package agenthttp

import (
	"context"
	"os"
	"path/filepath"
)

// RunCodex 以非交互方式执行 codex，并从 codex 写出的输出文件读取最终回复。
func RunCodex(body RunRequest, options RunnerOptions) (RunResult, error) {
	return RunCodexContext(context.Background(), body, options)
}

// RunCodexContext 以非交互方式执行 codex，并从 codex 写出的输出文件读取最终回复。
func RunCodexContext(ctx context.Context, body RunRequest, options RunnerOptions) (RunResult, error) {
	prep, err := prepareRunner(body, options, options.CodexCommand, "codex", "codex")
	if err != nil {
		return RunResult{}, err
	}

	tempDir, err := os.MkdirTemp("", "codex-http-")
	if err != nil {
		return RunResult{}, err
	}
	defer os.RemoveAll(tempDir)

	outputPath := filepath.Join(tempDir, "last-message.txt")
	childResult := runChild(ctx, prep.Prompt, prep.Timeout, execCommandSpec{
		Name: prep.Path,
		Args: []string{"exec", "--skip-git-repo-check", "-C", prep.Cwd, "-o", outputPath, "-"},
		Cwd:  prep.Cwd,
		Env:  prep.Env,
	})

	output, readErr := readOutputFile(outputPath)
	if readErr != nil {
		return RunResult{}, readErr
	}

	result, err := buildRunResult(childResult, "codex", output)
	if err != nil {
		return RunResult{}, err
	}
	return result, nil
}

// RunCodexStreamContext 以 SSE 兼容方式执行 codex。
// codex exec --json 当前不提供 Claude 风格的正文增量输出；流式接口改用
// experimental app-server 协议里的 item/agentMessage/delta 通知。
func RunCodexStreamContext(ctx context.Context, body RunRequest, writer StreamWriter, options RunnerOptions) (RunResult, error) {
	prep, err := prepareRunner(body, options, options.CodexCommand, "codex", "codex")
	if err != nil {
		return RunResult{}, err
	}

	appResult := runCodexAppServerStream(ctx, prep.Prompt, prep.Timeout, execCommandSpec{
		Name: prep.Path,
		Args: []string{"app-server", "--stdio"},
		Cwd:  prep.Cwd,
		Env:  prep.Env,
	}, writer, prep.Cwd, prep.AbsoluteWorkspaceRoot, normalizedCodexAppServerOptions(options.CodexAppServerOptions))

	return codexAppServerRunResult(appResult)
}

// codexRunResult 把 codex exec 子进程结果和输出文件内容转换成统一 RunResult。
func codexRunResult(child childResult, output string) (RunResult, error) {
	return buildRunResult(child, "codex", output)
}

// codexAppServerRun 保存一次 codex app-server 流式 turn 的执行状态。
type codexAppServerRun struct {
	// childResult 保存 app-server 进程退出信息和原始输出。
	childResult
	// Output 是 turn/completed 中最后一条 agentMessage 文本。
	Output string
	// TurnStatus 是 codex turn 的最终状态。
	TurnStatus string
	// TurnError 是 codex turn 失败时返回的错误文本。
	TurnError string
	// Initialized 标记 initialize 请求是否完成。
	Initialized bool
	// Completed 标记是否收到 turn/completed 通知。
	Completed bool
}

// codexAppServerRunResult 把 codex app-server 执行状态转换成统一 RunResult。
func codexAppServerRunResult(appResult codexAppServerRun) (RunResult, error) {
	result, err := buildRunResult(appResult.childResult, "codex", appResult.Output)
	if err != nil {
		return RunResult{}, err
	}
	if !result.OK {
		return result, nil
	}
	if !appResult.Completed {
		return RunResult{
			OK:       false,
			Error:    "codex app-server ended before turn completed",
			ExitCode: appResult.ExitCode,
			Output:   appResult.Output,
			Stdout:   appResult.Stdout,
			Stderr:   appResult.Stderr,
		}, nil
	}
	if appResult.TurnStatus != "" && appResult.TurnStatus != "completed" {
		message := appResult.TurnError
		if message == "" {
			message = "codex turn " + appResult.TurnStatus
		}
		return RunResult{
			OK:       false,
			Error:    message,
			ExitCode: appResult.ExitCode,
			Output:   appResult.Output,
			Stdout:   appResult.Stdout,
			Stderr:   appResult.Stderr,
		}, nil
	}

	return result, nil
}

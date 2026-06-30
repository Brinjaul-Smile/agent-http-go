package agenthttp

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
)

// RunCodex 以非交互方式执行 codex，并从 codex 写出的输出文件读取最终回复。
func RunCodex(body RunRequest, options RunnerOptions) (RunResult, error) {
	return RunCodexContext(context.Background(), body, options)
}

// RunCodexContext 以非交互方式执行 codex，并从 codex 写出的输出文件读取最终回复。
func RunCodexContext(ctx context.Context, body RunRequest, options RunnerOptions) (RunResult, error) {
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

	path, err := FindExecutable(runnerCommand(options.CodexCommand, "codex"), env)
	if err != nil {
		return RunResult{}, err
	}
	if path == "" {
		return RunResult{}, NewRequestError("codex CLI not found in PATH", http.StatusServiceUnavailable)
	}

	tempDir, err := os.MkdirTemp("", "codex-http-")
	if err != nil {
		return RunResult{}, err
	}
	defer os.RemoveAll(tempDir)

	outputPath := filepath.Join(tempDir, "last-message.txt")
	childResult := runChild(ctx, prompt, timeout, execCommandSpec{
		Name: path,
		Args: []string{"exec", "--skip-git-repo-check", "-C", cwd, "-o", outputPath, "-"},
		Cwd:  cwd,
		Env:  env,
	})

	output, readErr := readOutputFile(outputPath)
	if readErr != nil {
		return RunResult{}, readErr
	}

	if childResult.TimedOut {
		return RunResult{
			OK:       false,
			Error:    "codex execution timed out",
			ExitCode: childResult.ExitCode,
			TimedOut: true,
			Output:   output,
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
			Error:    "codex exited with code " + intString(code),
			ExitCode: childResult.ExitCode,
			Output:   output,
			Stdout:   childResult.Stdout,
			Stderr:   childResult.Stderr,
		}, nil
	}

	return RunResult{
		OK:       true,
		ExitCode: childResult.ExitCode,
		Output:   output,
		Stdout:   childResult.Stdout,
		Stderr:   childResult.Stderr,
	}, nil
}

// RunCodexStreamContext 以 SSE 兼容方式执行 codex。
// codex exec --json 当前不提供 Claude 风格的正文增量输出；流式接口改用
// experimental app-server 协议里的 item/agentMessage/delta 通知。
func RunCodexStreamContext(ctx context.Context, body RunRequest, writer StreamWriter, options RunnerOptions) (RunResult, error) {
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
	absoluteWorkspaceRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return RunResult{}, err
	}

	path, err := FindExecutable(runnerCommand(options.CodexCommand, "codex"), env)
	if err != nil {
		return RunResult{}, err
	}
	if path == "" {
		return RunResult{}, NewRequestError("codex CLI not found in PATH", http.StatusServiceUnavailable)
	}

	appResult := runCodexAppServerStream(ctx, prompt, timeout, execCommandSpec{
		Name: path,
		Args: []string{"app-server", "--stdio"},
		Cwd:  cwd,
		Env:  env,
	}, writer, cwd, absoluteWorkspaceRoot, normalizedCodexAppServerOptions(options.CodexAppServerOptions))

	return codexAppServerRunResult(appResult)
}

func codexRunResult(childResult childResult, output string) (RunResult, error) {
	if childResult.TimedOut {
		return RunResult{
			OK:       false,
			Error:    "codex execution timed out",
			ExitCode: childResult.ExitCode,
			TimedOut: true,
			Output:   output,
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
			Error:    "codex exited with code " + intString(code),
			ExitCode: childResult.ExitCode,
			Output:   output,
			Stdout:   childResult.Stdout,
			Stderr:   childResult.Stderr,
		}, nil
	}

	return RunResult{
		OK:       true,
		ExitCode: childResult.ExitCode,
		Output:   output,
		Stdout:   childResult.Stdout,
		Stderr:   childResult.Stderr,
	}, nil
}

type codexAppServerRun struct {
	childResult
	Output      string
	TurnStatus  string
	TurnError   string
	Initialized bool
	Completed   bool
}

func codexAppServerRunResult(appResult codexAppServerRun) (RunResult, error) {
	if appResult.TimedOut {
		return RunResult{
			OK:       false,
			Error:    "codex execution timed out",
			ExitCode: appResult.ExitCode,
			TimedOut: true,
			Output:   appResult.Output,
			Stdout:   appResult.Stdout,
			Stderr:   appResult.Stderr,
		}, nil
	}
	if appResult.Err != nil {
		return RunResult{}, appResult.Err
	}
	if appResult.ExitCode == nil || *appResult.ExitCode != 0 {
		code := 0
		if appResult.ExitCode != nil {
			code = *appResult.ExitCode
		}
		return RunResult{
			OK:       false,
			Error:    "codex exited with code " + intString(code),
			ExitCode: appResult.ExitCode,
			Output:   appResult.Output,
			Stdout:   appResult.Stdout,
			Stderr:   appResult.Stderr,
		}, nil
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

	return RunResult{
		OK:       true,
		ExitCode: appResult.ExitCode,
		Output:   appResult.Output,
		Stdout:   appResult.Stdout,
		Stderr:   appResult.Stderr,
	}, nil
}

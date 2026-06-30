package agenthttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// execCommandSpec 描述一次 agent CLI 子进程启动参数。
type execCommandSpec struct {
	// Name 是可执行文件路径。
	Name string
	// Args 是传给可执行文件的命令行参数。
	Args []string
	// Cwd 是子进程工作目录。
	Cwd string
	// Env 是子进程环境变量。
	Env []string
}

// childResult 保存子进程退出后的原始执行信息。
type childResult struct {
	// ExitCode 是子进程退出码；进程未正常启动或无法获取时为 nil。
	ExitCode *int
	// Stdout 是完整标准输出。
	Stdout string
	// Stderr 是完整标准错误。
	Stderr string
	// TimedOut 标记子进程是否因超时被终止。
	TimedOut bool
	// Err 保存启动、等待或流读取阶段的底层错误。
	Err error
}

// runChild 启动 CLI 子进程，把 prompt 写入 stdin，并收集 stdout/stderr。
func runChild(ctx context.Context, prompt string, timeout time.Duration, spec execCommandSpec) childResult {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := newChildCommand(spec)
	cmd.Stdin = strings.NewReader(prompt)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	result := childResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err := ctx.Err(); err != nil {
		result.Err = err
		return result
	}

	if err := cmd.Start(); err != nil {
		result.Err = err
		return result
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-errCh:
	case <-ctx.Done():
		waitErr = stopProcessGroup(cmd, errCh)
		if ctx.Err() == context.DeadlineExceeded {
			result.TimedOut = true
		} else {
			result.Err = ctx.Err()
		}
	}

	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	exitCode, resolveErr := resolveExitCode(waitErr, cmd)
	result.ExitCode = exitCode
	if resolveErr != nil && result.Err == nil {
		result.Err = resolveErr
	}
	return result
}

// runChildStream 启动 CLI 子进程，并把 stdout 读取到的片段实时写给 StreamWriter。
func runChildStream(ctx context.Context, prompt string, timeout time.Duration, spec execCommandSpec, writer StreamWriter) childResult {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := newChildCommand(spec)
	cmd.Stdin = strings.NewReader(prompt)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return childResult{Err: err}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return childResult{Err: err}
	}

	if err := ctx.Err(); err != nil {
		return childResult{Err: err}
	}
	if err := cmd.Start(); err != nil {
		return childResult{Err: err}
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	stdoutDone := make(chan error, 1)
	stderrDone := make(chan error, 1)

	go func() {
		stdoutDone <- copyStreamOutput(stdoutPipe, &stdout, writer)
	}()
	go func() {
		_, err := io.Copy(&stderr, stderrPipe)
		stderrDone <- err
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Wait()
	}()

	var waitErr error
	result := childResult{}
	select {
	case waitErr = <-errCh:
	case <-ctx.Done():
		waitErr = stopProcessGroup(cmd, errCh)
		if ctx.Err() == context.DeadlineExceeded {
			result.TimedOut = true
		} else {
			result.Err = ctx.Err()
		}
	}

	if err := <-stdoutDone; err != nil && result.Err == nil {
		result.Err = err
	}
	if err := <-stderrDone; err != nil && !isClosedPipeReadError(err) && result.Err == nil {
		result.Err = err
	}

	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	exitCode, resolveErr := resolveExitCode(waitErr, cmd)
	result.ExitCode = exitCode
	if resolveErr != nil && result.Err == nil {
		result.Err = resolveErr
	}

	return result
}

// copyStreamOutput 从 stdout pipe 复制数据，并把读取到的 chunk 转发给 StreamWriter。
func copyStreamOutput(reader io.Reader, stdout *bytes.Buffer, writer StreamWriter) error {
	if flusher, ok := writer.(interface{ Flush() error }); ok {
		defer func() {
			_ = flusher.Flush()
		}()
	}

	buffer := make([]byte, 1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			chunk := string(buffer[:n])
			stdout.WriteString(chunk)
			if writer != nil {
				if writeErr := writer.WriteDelta(chunk); writeErr != nil {
					return writeErr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if isClosedPipeReadError(err) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// isClosedPipeReadError 判断错误是否为进程退出导致的 pipe 正常关闭。
func isClosedPipeReadError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrClosed) || strings.Contains(err.Error(), "file already closed")
}

// stopProcessGroup 先终止子进程组，超时后强制杀掉整个进程组。
func stopProcessGroup(cmd *exec.Cmd, errCh <-chan error) error {
	if cmd.Process == nil {
		return <-errCh
	}

	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-time.After(time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return <-errCh
	}
}

// readOutputFile 读取 codex -o 参数写出的最终消息文件。
func readOutputFile(outputPath string) (string, error) {
	payload, err := os.ReadFile(outputPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(payload), nil
}

// newChildCommand 创建配置了独立进程组的 CLI 子进程，统一 runChild、runChildStream
// 和 runCodexAppServerStream 中重复的 exec.Command 构造。
func newChildCommand(spec execCommandSpec) *exec.Cmd {
	cmd := exec.Command(spec.Name, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = spec.Env
	// 将 CLI 放到独立进程组里，取消时可以一起终止 shell wrapper 和子进程。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// resolveExitCode 从子进程等待错误中提取退出码，统一 runChild、runChildStream
// 和 finishCodexAppServerRun 中相同的退出码解析逻辑。
func resolveExitCode(waitErr error, cmd *exec.Cmd) (*int, error) {
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code := exitErr.ExitCode()
		return &code, nil
	}
	if waitErr != nil {
		return nil, waitErr
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		return &code, nil
	}
	return nil, nil
}

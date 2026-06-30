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

type execCommandSpec struct {
	Name string
	Args []string
	Cwd  string
	Env  []string
}

// childResult 保存子进程退出后的原始执行信息。
type childResult struct {
	ExitCode *int
	Stdout   string
	Stderr   string
	TimedOut bool
	Err      error
}

// runChild 启动 CLI 子进程，把 prompt 写入 stdin，并收集 stdout/stderr。
func runChild(ctx context.Context, prompt string, timeout time.Duration, spec execCommandSpec) childResult {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(spec.Name, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = spec.Env
	cmd.Stdin = strings.NewReader(prompt)

	// 将 CLI 放到独立进程组里，取消时可以一起终止 shell wrapper 和子进程，
	// 避免只杀掉 exec 启动的第一个进程。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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

	var err error
	select {
	case err = <-errCh:
	case <-ctx.Done():
		err = stopProcessGroup(cmd, errCh)
		if ctx.Err() == context.DeadlineExceeded {
			result.TimedOut = true
		} else {
			result.Err = ctx.Err()
		}
	}

	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		result.ExitCode = &code
		return result
	}
	if err != nil {
		result.Err = err
		return result
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.ExitCode = &code
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

	cmd := exec.Command(spec.Name, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = spec.Env
	cmd.Stdin = strings.NewReader(prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code := exitErr.ExitCode()
		result.ExitCode = &code
		return result
	}
	if waitErr != nil && result.Err == nil {
		result.Err = waitErr
		return result
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.ExitCode = &code
	}

	return result
}

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

func isClosedPipeReadError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrClosed) || strings.Contains(err.Error(), "file already closed")
}

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

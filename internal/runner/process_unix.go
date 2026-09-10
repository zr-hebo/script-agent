//go:build linux || darwin

package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func runProcess(ctx context.Context, cmd *exec.Cmd, grace time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Bound waits when background children inherit output pipes.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	killGroup := func(signal syscall.Signal) { _ = syscall.Kill(-pid, signal) }
	// Scripts must not leave detached work in their original process group.
	defer killGroup(syscall.SIGKILL)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		killGroup(syscall.SIGTERM)
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			killGroup(syscall.SIGKILL)
			<-done
		}
		return ctx.Err()
	}
}

// Do not let a script make the supervisor block on a FIFO or follow a symlink.
func readRegularFile(path string, limit int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("result must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, err
}

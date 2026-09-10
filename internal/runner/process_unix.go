//go:build linux || darwin

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type lifecycleState struct {
	Err             error
	RunInterrupted  error
	PostInterrupted error
	PostStarted     bool
}

// A dedicated, bounded pipe reports phase transitions; stdout remains user logs.
// Once PostRun starts, it has its own deadline and ignores caller cancellation.
func runLifecycleProcess(ctx context.Context, cmd *exec.Cmd, grace, postTimeout time.Duration, phase func(string)) (state lifecycleState) {
	if ctx.Err() != nil {
		state.RunInterrupted = ctx.Err()
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		state.Err = err
		return
	}
	defer r.Close()
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		w.Close()
		state.Err = err
		return
	}
	w.Close()
	pid := cmd.Process.Pid
	kill := func(sig syscall.Signal) { _ = syscall.Kill(-pid, sig) }
	defer kill(syscall.SIGKILL)
	events := make(chan string, 2)
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		defer close(events)
		decoder := json.NewDecoder(io.LimitReader(r, 4096))
		for count := 0; count < 2; count++ {
			var event struct{ Phase string }
			if decoder.Decode(&event) != nil {
				return
			}
			if event.Phase != "run" && event.Phase != "post-run" {
				return
			}
			events <- event.Phase
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ctxDone := ctx.Done()
	var timer *time.Timer
	var tick <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	setTimer := func(d time.Duration) {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(d)
		tick = timer.C
	}
	runSeen, killing := false, false
	accept := func(name string) {
		if state.PostStarted {
			return
		}
		if name == "run" {
			if !runSeen {
				runSeen = true
				phase(name)
			}
			return
		}
		state.PostStarted = true
		if state.RunInterrupted == nil && ctx.Err() != nil {
			state.RunInterrupted = ctx.Err()
		}
		ctxDone, killing = nil, false
		setTimer(postTimeout)
		phase(name)
	}
	for {
		select {
		case name, ok := <-events:
			if !ok {
				events = nil
			} else {
				accept(name)
			}
		case state.Err = <-done:
			// Drain transitions emitted immediately before a fast process exited.
			select {
			case <-eventsDone:
			case <-time.After(50 * time.Millisecond):
				r.Close()
				<-eventsDone
			}
			if events != nil {
				for name := range events {
					accept(name)
				}
			}
			return
		case <-ctxDone:
			state.RunInterrupted = ctx.Err()
			_ = os.WriteFile(filepath.Join(cmd.Dir, "interrupt.status"), []byte(interrupted(ctx.Err()).Status), 0600)
			ctxDone = nil
			kill(syscall.SIGTERM)
			killing = true
			setTimer(grace)
		case <-tick:
			if state.PostStarted && !killing {
				state.PostInterrupted = context.DeadlineExceeded
				kill(syscall.SIGTERM)
				killing = true
				setTimer(grace)
			} else {
				kill(syscall.SIGKILL)
				tick = nil
			}
		}
	}
}

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

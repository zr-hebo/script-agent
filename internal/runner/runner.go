package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Runner launches every script in a new process. This is NOT a security sandbox.
// GoExecutable must be a script-agent binary supporting the internal-go command.
type Runner struct {
	GoExecutable string
	BashPath     string
	PythonPath   string
	GracePeriod  time.Duration
}

func New() (*Runner, error) {
	return &Runner{GoExecutable: "script-agent", BashPath: "bash", PythonPath: "python3", GracePeriod: time.Second}, nil
}

func (r *Runner) Execute(ctx context.Context, request Request) (result Result) {
	return r.ExecuteWithPhase(ctx, request, func(string) {})
}

func (r *Runner) ExecuteWithPhase(ctx context.Context, request Request, phase func(string)) (result Result) {
	phase("prepare")
	start := time.Now()
	defer func() { result.DurationMS = time.Since(start).Milliseconds() }()
	req, err := request.Normalize()
	if err != nil {
		return failure("invalid_request", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeoutDuration(req))
	defer cancel()
	if ctx.Err() != nil {
		return interrupted(ctx.Err())
	}
	dir, err := os.MkdirTemp("", "script-agent-")
	if err != nil {
		return failure("setup_error", err)
	}
	defer os.RemoveAll(dir)
	params, _ := json.Marshal(req.Params) // checked by Normalize
	sourcePath := filepath.Join(dir, "script."+map[string]string{"go": "go", "shell": "sh", "python": "py"}[req.Language])
	paramsPath := filepath.Join(dir, "params.json")
	resultPath := filepath.Join(dir, "result.json")
	errorPath := filepath.Join(dir, "error.json")
	for path, data := range map[string][]byte{sourcePath: []byte(req.Source), paramsPath: params} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			return failure("setup_error", err)
		}
	}
	var cmd *exec.Cmd
	switch req.Language {
	case "go":
		cmd = exec.Command(r.GoExecutable, "internal-go", sourcePath, paramsPath, resultPath, errorPath)
	case "shell":
		cmd = exec.Command(r.BashPath, "--noprofile", "--norc", "-E", "-e", "-u", "-o", "pipefail", "-c", shellEntry, "script-agent", sourcePath)
	case "python":
		wrapper := filepath.Join(dir, "python_entry.py")
		if err := os.WriteFile(wrapper, []byte(pythonEntry), 0600); err != nil {
			return failure("setup_error", err)
		}
		cmd = exec.Command(r.PythonPath, "-I", "-u", wrapper, sourcePath, paramsPath, resultPath, errorPath)
	}
	cmd.Dir = dir
	// Deliberately do not inherit service tokens, proxy credentials, PYTHONPATH,
	// BASH_ENV, or other ambient environment. This is hygiene, not isolation.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir,
		"LANG=C", "LC_ALL=C",
		"SCRIPT_PARAMS_FILE=" + paramsPath, "BATCH_PARAMS_FILE=" + paramsPath,
		"SCRIPT_RESULT_FILE=" + resultPath,
	}
	stdout, stderr := &limitedLog{}, &limitedLog{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	phase("run")
	err = runProcess(ctx, cmd, r.GracePeriod)
	result.Stdout, result.StdoutTruncated = stdout.text(), stdout.truncated
	result.Stderr, result.StderrTruncated = stderr.text(), stderr.truncated
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.ExitCode = &code
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		state := interrupted(err)
		result.Status, result.Error = state.Status, state.Error
		return result
	}
	if err != nil {
		result.Status = "failed"
		result.Error = &ExecutionError{Type: "process_error", Message: err.Error()}
		// Structured child diagnostics are best effort; exit status stays authoritative.
		if data, readErr := readRegularFile(errorPath, MaxLogBytes); readErr == nil {
			var detail ExecutionError
			if json.Unmarshal(data, &detail) == nil && detail.Type != "" && detail.Message != "" {
				result.Error = &detail
			}
		}
		return result
	}
	data, err := readRegularFile(resultPath, MaxResultBytes)
	if errors.Is(err, os.ErrNotExist) && req.Language == "shell" {
		result.Status = "succeeded" // shell's structured result is optional
		return result
	}
	if err == nil {
		err = json.Unmarshal(data, &result.Data)
	}
	if err != nil {
		result.Status = "failed"
		result.Error = &ExecutionError{Type: "result_error", Message: fmt.Sprintf("invalid or missing result JSON: %v", err)}
		return result
	}
	result.Status = "succeeded"
	return result
}

func interrupted(err error) Result {
	status := "cancelled"
	if errors.Is(err, context.DeadlineExceeded) {
		status = "timed_out"
	}
	return Result{Status: status, Error: &ExecutionError{Type: status, Message: err.Error()}}
}

type limitedLog struct {
	data      []byte
	truncated bool
}

func (b *limitedLog) Write(p []byte) (int, error) {
	n := len(p)
	remaining := MaxLogBytes - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil // keep draining even after the cap
}

func (b *limitedLog) text() string { return string(b.data) }

const shellEntry = `trap 'rc=$?; printf "script error: line=%s exit=%s\n" "$LINENO" "$rc" >&2; exit "$rc"' ERR
source "$1"`

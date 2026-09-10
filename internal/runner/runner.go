package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/zr-hebo/script-agent/sdk"
)

// Runner launches user code only in disposable processes, not a security sandbox.
type Runner struct {
	GoExecutable string
	BashPath     string
	PythonPath   string
	GracePeriod  time.Duration
	LogWriter    io.Writer
}

func New() (*Runner, error) {
	return &Runner{GoExecutable: "script-agent", BashPath: "bash", PythonPath: "python3", GracePeriod: time.Second}, nil
}

func (r *Runner) ExecuteWithPhase(ctx context.Context, request Request, phase func(string)) (result Result) {
	start := time.Now()
	result.UserPostRun.Status = "skipped"
	defer func() { result.DurationMS = time.Since(start).Milliseconds() }()
	req, err := request.Normalize()
	if err != nil {
		return failure("invalid_request", err)
	}
	var paramEnv []string
	if req.Language == "shell" {
		paramEnv, err = paramsEnvironment(req.Params)
		if err != nil {
			return failure("invalid_request", err)
		}
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
	params, _ := json.Marshal(req.Params)
	sourcePath := filepath.Join(dir, "script."+map[string]string{"go": "go", "shell": "sh", "python": "py"}[req.Language])
	paramsPath := filepath.Join(dir, "params.json")
	resultPath := filepath.Join(dir, "result.json")
	postPath := filepath.Join(dir, "post-result.json")
	for path, data := range map[string][]byte{sourcePath: []byte(req.Source), paramsPath: params} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			return failure("setup_error", err)
		}
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir, "LANG=C", "LC_ALL=C",
		"SCRIPT_PARAMS_FILE=" + paramsPath, "BATCH_PARAMS_FILE=" + paramsPath,
		"SCRIPT_RESULT_FILE=" + resultPath, "SCRIPT_OUTCOME_FILE=" + filepath.Join(dir, "outcome.json"),
		"SCRIPT_POST_RUN_TIMEOUT_SECONDS=" + strconv.Itoa(req.PostRunTimeoutSeconds),
		"SCRIPT_INTERRUPT_FILE=" + filepath.Join(dir, "interrupt.status"),
	}
	env = append(env, paramEnv...)
	stdout, stderr := &limitedLog{stream: r.LogWriter}, &limitedLog{stream: r.LogWriter}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return failure("setup_error", err)
	}
	prefix := "\x1escript-agent:" + hex.EncodeToString(token[:]) + ":"
	env = append(env, "SCRIPT_LOG_MARKER="+prefix)
	logs := newPhaseLogStore()
	output, errorOutput := newPhaseLogWriter(stdout, logs, "stdout", prefix), newPhaseLogWriter(stderr, logs, "stderr", prefix)
	defer func() {
		output.close()
		errorOutput.close()
		result.PhaseLogs = logs.phases
		result.Stdout, result.StdoutTruncated = stdout.text(), stdout.truncated
		result.Stderr, result.StderrTruncated = stderr.text(), stderr.truncated
	}()
	if req.Language == "shell" {
		return r.executeShell(ctx, req, dir, env, output, errorOutput, phase)
	}
	var cmd *exec.Cmd
	if req.Language == "go" {
		cmd = exec.Command(r.GoExecutable, "internal-go", sourcePath, paramsPath, resultPath, postPath)
	} else {
		wrapper := filepath.Join(dir, "python_entry.py")
		if err := os.WriteFile(wrapper, []byte(pythonEntry), 0600); err != nil {
			return failure("setup_error", err)
		}
		cmd = exec.Command(r.PythonPath, "-I", "-u", wrapper, sourcePath, paramsPath, resultPath, postPath)
	}
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, output, errorOutput
	state := runLifecycleProcess(ctx, cmd, r.GracePeriod, time.Duration(req.PostRunTimeoutSeconds)*time.Second, phase)
	out, readErr := readOutcome(resultPath, false)
	if readErr != nil {
		result = failure("result_error", fmt.Errorf("invalid or missing result JSON: %w", readErr))
	} else {
		result.Outcome = out
	}
	if state.RunInterrupted != nil {
		result.Outcome = interrupted(state.RunInterrupted).Outcome
	} else if state.Err != nil && (!state.PostStarted || readErr != nil) {
		// A structured load/prepare error is useful, but process failure cannot
		// turn a previously claimed success into an actual success.
		if readErr != nil || result.Status == sdk.StatusSucceeded {
			result = failure("process_error", state.Err)
		}
	}
	if state.PostStarted {
		post, err := readOutcome(postPath, true)
		switch {
		case state.PostInterrupted != nil:
			result.UserPostRun = interrupted(state.PostInterrupted).Outcome
		case err != nil || state.Err != nil:
			result.UserPostRun = sdk.Outcome{Status: sdk.StatusFailed, Error: fmt.Errorf("post-run process/result failure: process=%v result=%v", state.Err, err)}
		default:
			result.UserPostRun = post
		}
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.ExitCode = &code
	}
	return
}

func readOutcome(path string, allowSkipped bool) (sdk.Outcome, error) {
	data, err := readRegularFile(path, MaxResultBytes)
	if err != nil {
		return sdk.Outcome{}, err
	}
	var out sdk.Outcome
	if err = json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	if allowSkipped && out.Status == "skipped" && out.Error == nil {
		return out, nil
	}
	return out, out.Validate()
}

func (r *Runner) executeShell(ctx context.Context, req Request, dir string, env []string, stdout, stderr *phaseLogWriter, phase func(string)) (result Result) {
	result.UserPostRun.Status = "skipped"
	runStage := func(ctx context.Context, name, source string) sdk.Outcome {
		stdout.setPhase(name)
		stderr.setPhase(name)
		defer stdout.close()
		defer stderr.close()
		path := filepath.Join(dir, name+".sh")
		if err := os.WriteFile(path, []byte(source), 0600); err != nil {
			return failure("setup_error", err).Outcome
		}
		cmd := exec.Command(r.BashPath, "--noprofile", "--norc", "-E", "-e", "-u", "-o", "pipefail", "-c", shellEntry, "script-agent", path)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, stdout, stderr
		err := runProcess(ctx, cmd, r.GracePeriod)
		out := sdk.Outcome{Status: sdk.StatusSucceeded}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			out = interrupted(err).Outcome
		} else if err != nil {
			out = failure(name, err).Outcome
		}
		if cmd.ProcessState != nil {
			code := cmd.ProcessState.ExitCode()
			out.ExitCode = &code
		}
		return out
	}
	result.Outcome = sdk.Outcome{Status: sdk.StatusSucceeded}
	if req.PrepareSource != "" {
		result.Outcome = runStage(ctx, "prepare", req.PrepareSource)
	}
	if ctx.Err() != nil {
		result.Outcome = interrupted(ctx.Err()).Outcome
	}
	if result.Status == sdk.StatusSucceeded {
		phase("run")
		// A Prepare script cannot supply a stale Run result.
		_ = os.Remove(filepath.Join(dir, "result.json"))
		result.Outcome = runStage(ctx, "run", req.Source)
		if result.Status == sdk.StatusSucceeded {
			data, err := readRegularFile(filepath.Join(dir, "result.json"), MaxResultBytes)
			if err == nil {
				err = json.Unmarshal(data, &result.Data)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				result.Status = sdk.StatusFailed
				result.Error = fmt.Errorf("invalid result JSON: %w", err)
			}
		}
	}
	result.Stdout, result.Stderr = stdout.capture.text(), stderr.capture.text()
	result.StdoutTruncated, result.StderrTruncated = stdout.capture.truncated, stderr.capture.truncated
	phase("post-run")
	if req.PostRunSource != "" {
		if err := writeOutcome(filepath.Join(dir, "outcome.json"), result.Outcome); err != nil {
			result.UserPostRun = failure("post-run input", err).Outcome
		} else {
			postCtx, cancel := context.WithTimeout(context.Background(), time.Duration(req.PostRunTimeoutSeconds)*time.Second)
			defer cancel()
			result.UserPostRun = runStage(postCtx, "post-run", req.PostRunSource)
		}
	}
	return
}

func interrupted(err error) Result {
	status := sdk.StatusCancelled
	if errors.Is(err, context.DeadlineExceeded) {
		status = sdk.StatusTimedOut
	}
	return Result{Outcome: sdk.Outcome{Status: status, Error: err}, UserPostRun: sdk.Outcome{Status: "skipped"}}
}

type limitedLog struct {
	data      []byte
	truncated bool
	stream    io.Writer
}

func (b *limitedLog) Write(p []byte) (int, error) {
	n := len(p)
	if b.stream != nil {
		// Streaming failure must not stop draining the child's output pipes.
		_, _ = b.stream.Write(p)
	}
	remaining := MaxLogBytes - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil
}
func (b *limitedLog) text() string { return string(b.data) }

const shellEntry = `trap 'rc=$?; printf "script error: line=%s exit=%s\n" "$LINENO" "$rc" >&2; exit "$rc"' ERR
source "$1"`

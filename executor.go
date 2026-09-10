// Package scriptagent executes Go (Yaegi), Bash and Python scripts through the
// prepare -> run -> post-run lifecycle. User code always runs in a child process.
// This package does not provide a security sandbox, persistence, or scheduling.
package scriptagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/zr-hebo/script-agent/internal/runner"
	"github.com/zr-hebo/script-agent/sdk"
)

type Request = runner.Request
type Outcome = sdk.Outcome
type TaskRunner = sdk.TaskRunner
type BaseTask = sdk.BaseTask
type Status = sdk.Status

const (
	StatusSucceeded = sdk.StatusSucceeded
	StatusFailed    = sdk.StatusFailed
	StatusTimedOut  = sdk.StatusTimedOut
	StatusCancelled = sdk.StatusCancelled
)

type Config struct {
	// Path to the standalone script-agent binary used as a disposable Go helper.
	// Empty means resolve script-agent from PATH; no HTTP server is needed.
	GoExecutable string
	BashPath     string
	PythonPath   string
	GracePeriod  time.Duration
	Callback     CallbackConfig
}

type PhaseResult struct {
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

type Result struct {
	TaskID      string         `json:"task_id"`
	ExecutionID string         `json:"execution_id"`
	Status      string         `json:"status"`
	Phase       string         `json:"phase"`
	Phases      []PhaseResult  `json:"phases"`
	Outcome     Outcome        `json:"outcome"`
	UserPostRun Outcome        `json:"user_post_run"`
	Callback    CallbackResult `json:"callback"`
}

type Executor struct {
	runner   *runner.Runner
	callback *callbackSender
}

func New(config Config) (*Executor, error) {
	r, err := runner.New()
	if err != nil {
		return nil, err
	}
	if config.GoExecutable != "" {
		r.GoExecutable = config.GoExecutable
	}
	if config.BashPath != "" {
		r.BashPath = config.BashPath
	}
	if config.PythonPath != "" {
		r.PythonPath = config.PythonPath
	}
	// Child working directories are per-execution; resolve explicit relative
	// executable paths against the caller's directory before changing directories.
	for _, path := range []*string{&r.GoExecutable, &r.BashPath, &r.PythonPath} {
		if strings.ContainsRune(*path, '/') {
			absolute, err := filepath.Abs(*path)
			if err != nil {
				return nil, err
			}
			*path = absolute
		}
	}
	if config.GracePeriod < 0 {
		return nil, fmt.Errorf("grace period cannot be negative")
	}
	if config.GracePeriod > 0 {
		r.GracePeriod = config.GracePeriod
	}
	callback, err := newCallbackSender(config.Callback)
	if err != nil {
		return nil, err
	}
	return &Executor{runner: r, callback: callback}, nil
}

// HandleHelperCommand lets an embedding application reuse its own executable as
// the isolated Go helper. Call this before starting servers, loading credentials,
// or other application initialization in main, with os.Args[1:]. Pass the current
// executable path in Config.GoExecutable. ctx should observe SIGTERM/SIGINT.
// It does not intercept commands automatically or start an HTTP server.
func HandleHelperCommand(ctx context.Context, args []string) (handled bool, exitCode int) {
	if len(args) == 0 || args[0] != "internal-go" {
		return false, 0
	}
	if len(args) != 5 {
		return true, 2
	}
	return true, runner.GoEntry(ctx, args[1], args[2], args[3], args[4])
}

// Validate snapshots JSON parameters and rejects unapproved callback origins.
func (e *Executor) Validate(req Request) (Request, error) {
	if len(req.TaskID) > 256 {
		return req, fmt.Errorf("task_id exceeds 256 bytes")
	}
	if err := e.callback.validateURL(req.CallbackURL); err != nil {
		return req, err
	}
	return req.Normalize()
}

func NewExecutionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Execute blocks through post-run. Cancelling ctx stops the script, but post-run
// callback delivery uses its own bounded context so cancellation is still reported.
func (e *Executor) Execute(ctx context.Context, req Request) Result {
	return e.ExecuteWithObserver(ctx, NewExecutionID(), req, nil)
}

// ExecuteWithObserver reports phase transitions synchronously. The observer must
// return quickly and must not mutate the request. executionID is a correlation key,
// not an idempotency guarantee; Execute itself never retries user code.
func (e *Executor) ExecuteWithObserver(ctx context.Context, executionID string, req Request, observe func(string)) Result {
	result := Result{
		TaskID: req.TaskID, ExecutionID: executionID,
		Phases:   []PhaseResult{{Name: "prepare", Status: "pending"}, {Name: "run", Status: "pending"}, {Name: "post-run", Status: "pending"}},
		Callback: CallbackResult{Status: "skipped"},
	}
	current := -1
	transition := func(name string) {
		now := time.Now().UTC()
		if current >= 0 {
			result.Phases[current].FinishedAt = &now
			if result.Phases[current].Status == "running" {
				result.Phases[current].Status = "succeeded"
			}
		}
		for index := range result.Phases {
			if result.Phases[index].Name == name {
				current = index
				result.Phases[index].StartedAt = &now
				result.Phases[index].Status = "running"
				break
			}
		}
		result.Phase = name
		if observe != nil {
			observe(name)
		}
	}
	transition("prepare")
	result.UserPostRun.Status = "skipped"
	normalized, err := e.Validate(req)
	if err != nil {
		result.Outcome = Outcome{Status: StatusFailed, Error: fmt.Errorf("invalid_request: %w", err)}
	} else {
		report := e.runner.ExecuteWithPhase(ctx, normalized, func(name string) {
			if name == "post-run" && current == 0 {
				result.Phases[1].Status = "skipped"
			}
			transition(name)
		})
		result.Outcome, result.UserPostRun = report.Outcome, report.UserPostRun
	}
	result.Status = string(result.Outcome.Status)
	primaryPhase := 0
	if result.Phases[1].StartedAt != nil {
		primaryPhase = 1
	}
	result.Phases[primaryPhase].Status = result.Status
	if result.Phases[1].StartedAt == nil {
		result.Phases[1].Status = "skipped"
	}
	if current != 2 {
		transition("post-run")
	}
	if req.CallbackURL != "" {
		// Delivery is deliberately independent of the cancelled execution context.
		result.Callback = e.callback.send(req.CallbackURL, CallbackEvent{
			Event: "task.completed", TaskID: req.TaskID, ExecutionID: executionID,
			Phase: "post-run", Status: result.Status, Outcome: result.Outcome, UserPostRun: result.UserPostRun,
		})
	}
	now := time.Now().UTC()
	result.Phases[2].FinishedAt = &now
	result.Phases[2].Status = "succeeded"
	if result.Callback.Status == "failed" || (result.UserPostRun.Status != "succeeded" && result.UserPostRun.Status != "skipped") {
		result.Phases[2].Status = "failed"
	}
	return result
}

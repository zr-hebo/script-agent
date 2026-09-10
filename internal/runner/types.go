package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	MaxSourceBytes = 256 << 10
	MaxParamsBytes = 64 << 10
	MaxResultBytes = 1 << 20
	MaxLogBytes    = 64 << 10
	DefaultTimeout = 300
	MaxTimeout     = 3600
)

type Request struct {
	TaskID         string         `json:"task_id,omitempty"`
	CallbackURL    string         `json:"callback_url,omitempty"`
	Language       string         `json:"language"`
	Source         string         `json:"source"`
	Params         map[string]any `json:"params"`
	TimeoutSeconds int            `json:"timeout_seconds"`
}

// Normalize also snapshots Params, so concurrent executions do not share maps.
func (r Request) Normalize() (Request, error) {
	if r.Language != "go" && r.Language != "shell" && r.Language != "python" {
		return r, fmt.Errorf("language must be go, shell or python")
	}
	if strings.TrimSpace(r.Source) == "" || len(r.Source) > MaxSourceBytes {
		return r, fmt.Errorf("source must contain 1..%d bytes", MaxSourceBytes)
	}
	if r.TimeoutSeconds == 0 {
		r.TimeoutSeconds = DefaultTimeout
	}
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > MaxTimeout {
		return r, fmt.Errorf("timeout_seconds must be 1..%d", MaxTimeout)
	}
	if r.Params == nil {
		r.Params = map[string]any{}
	}
	data, err := json.Marshal(r.Params)
	if err != nil {
		return r, fmt.Errorf("params must be JSON serializable: %w", err)
	}
	if len(data) > MaxParamsBytes {
		return r, fmt.Errorf("params exceeds %d bytes", MaxParamsBytes)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return r, err
	}
	r.Params = snapshot
	return r, nil
}

type ExecutionError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
}

type Result struct {
	Status          string          `json:"status"`
	Data            map[string]any  `json:"data"`
	Error           *ExecutionError `json:"error"`
	ExitCode        *int            `json:"exit_code"`
	Stdout          string          `json:"stdout"`
	Stderr          string          `json:"stderr"`
	StdoutTruncated bool            `json:"stdout_truncated"`
	StderrTruncated bool            `json:"stderr_truncated"`
	DurationMS      int64           `json:"duration_ms"`
}

func failure(kind string, err error) Result {
	return Result{Status: "failed", Error: &ExecutionError{Type: kind, Message: err.Error()}}
}

func timeoutDuration(r Request) time.Duration {
	return time.Duration(r.TimeoutSeconds) * time.Second
}

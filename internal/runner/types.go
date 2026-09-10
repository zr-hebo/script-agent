package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zr-hebo/script-agent/sdk"
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
	TaskID                string         `json:"task_id,omitempty"`
	CallbackURL           string         `json:"callback_url,omitempty"`
	Language              string         `json:"language"`
	Source                string         `json:"source"`
	PrepareSource         string         `json:"prepare_source,omitempty"`
	PostRunSource         string         `json:"post_run_source,omitempty"`
	Params                map[string]any `json:"params"`
	TimeoutSeconds        int            `json:"timeout_seconds"`
	PostRunTimeoutSeconds int            `json:"post_run_timeout_seconds,omitempty"`
}

// Normalize also snapshots Params, so concurrent executions do not share maps.
func (r Request) Normalize() (Request, error) {
	if r.Language != "go" && r.Language != "shell" && r.Language != "python" {
		return r, fmt.Errorf("language must be go, shell or python")
	}
	if strings.TrimSpace(r.Source) == "" || len(r.Source)+len(r.PrepareSource)+len(r.PostRunSource) > MaxSourceBytes {
		return r, fmt.Errorf("source must contain 1..%d bytes", MaxSourceBytes)
	}
	if r.Language != "shell" && (r.PrepareSource != "" || r.PostRunSource != "") {
		return r, fmt.Errorf("prepare_source and post_run_source apply only to shell")
	}
	if r.PostRunTimeoutSeconds == 0 {
		r.PostRunTimeoutSeconds = 10
	}
	if r.PostRunTimeoutSeconds < 1 || r.PostRunTimeoutSeconds > 60 {
		return r, fmt.Errorf("post_run_timeout_seconds must be 1..60")
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

type Result struct {
	sdk.Outcome
	UserPostRun sdk.Outcome
}

func failure(kind string, err error) Result {
	return Result{Outcome: sdk.Outcome{Status: sdk.StatusFailed, Error: fmt.Errorf("%s: %w", kind, err)}, UserPostRun: sdk.Outcome{Status: "skipped"}}
}

func timeoutDuration(r Request) time.Duration {
	return time.Duration(r.TimeoutSeconds) * time.Second
}

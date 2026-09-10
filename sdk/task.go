// Package sdk is the small, interpreter-safe contract shared with user Go code.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type Status string

const (
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusTimedOut  Status = "timed_out"
	StatusCancelled Status = "cancelled"
)

type TaskRunner interface {
	Prepare(context.Context, map[string]any) error
	Run(context.Context, map[string]any) Outcome
	PostRun(context.Context, map[string]any, Outcome) error
}

// BaseTask supplies optional lifecycle hooks. Embed it and implement only Run.
type BaseTask struct{}

func (BaseTask) Prepare(context.Context, map[string]any) error          { return nil }
func (BaseTask) PostRun(context.Context, map[string]any, Outcome) error { return nil }

// Outcome is shared by Run, PostRun, the package API and HTTP callbacks.
// Only Status, Data and Error are supplied by user Run; the supervisor owns
// process/log metadata. Across JSON boundaries errors retain their message only.
type Outcome struct {
	Status          Status         `json:"status"`
	Data            map[string]any `json:"data"`
	Error           error          `json:"-"`
	Stack           string         `json:"stack,omitempty"`
	ExitCode        *int           `json:"exit_code"`
	Stdout          string         `json:"stdout"`
	Stderr          string         `json:"stderr"`
	StdoutTruncated bool           `json:"stdout_truncated"`
	StderrTruncated bool           `json:"stderr_truncated"`
	DurationMS      int64          `json:"duration_ms"`
}

func (o Outcome) MarshalJSON() ([]byte, error) {
	type plain Outcome
	var message *string
	if o.Error != nil {
		value := o.Error.Error()
		message = &value
	}
	return json.Marshal(struct {
		plain
		Error *string `json:"error"`
	}{plain(o), message})
}

func (o *Outcome) UnmarshalJSON(data []byte) error {
	type plain Outcome
	var value struct {
		plain
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*o = Outcome(value.plain)
	if value.Error != nil {
		o.Error = errors.New(*value.Error)
	}
	return nil
}

// Validate rejects ambiguous or contradictory user results.
func (o Outcome) Validate() error {
	switch o.Status {
	case StatusSucceeded:
		if o.Error != nil {
			return errors.New("succeeded outcome cannot contain an error")
		}
	case StatusFailed, StatusTimedOut, StatusCancelled:
		if o.Error == nil || o.Error.Error() == "" {
			return errors.New("unsuccessful outcome must contain an error")
		}
	default:
		return fmt.Errorf("invalid outcome status %q", o.Status)
	}
	return nil
}

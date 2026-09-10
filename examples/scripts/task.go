// Upload this file as a Go source. Prepare and PostRun are optional overrides.
package usercode

import (
	"context"
	"fmt"

	"github.com/zr-hebo/script-agent/sdk"
)

type Task struct {
	sdk.BaseTask
	clusterUUID string
}

func New() sdk.TaskRunner { return &Task{} }

func (t *Task) Prepare(ctx context.Context, params map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	uuid, ok := params["cluster_uuid"].(string)
	if !ok || uuid == "" {
		return fmt.Errorf("cluster_uuid is required")
	}
	t.clusterUUID = uuid
	return nil
}

func (t *Task) Run(ctx context.Context, params map[string]any) sdk.Outcome {
	if err := ctx.Err(); err != nil {
		return sdk.Outcome{Status: sdk.StatusCancelled, Error: err}
	}
	fmt.Println("processing", t.clusterUUID)
	return sdk.Outcome{Status: sdk.StatusSucceeded, Data: map[string]any{"cluster_uuid": t.clusterUUID}}
}

func (t *Task) PostRun(ctx context.Context, params map[string]any, outcome sdk.Outcome) error {
	// This may run after Prepare failed: do not assume resources were allocated.
	fmt.Println("cleanup after", outcome.Status)
	return ctx.Err()
}

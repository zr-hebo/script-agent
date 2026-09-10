package usercode

import (
	"context"
	"fmt"

	"github.com/zr-hebo/script-agent/sdk"
)

// Task inherits no-op Prepare and PostRun hooks; override them when needed.
type Task struct {
	sdk.BaseTask
}

func New() sdk.TaskRunner { return &Task{} }

func (t *Task) Run(ctx context.Context, params map[string]any) sdk.Outcome {
	if err := ctx.Err(); err != nil {
		return sdk.Outcome{Status: sdk.StatusCancelled, Error: err}
	}
	clusterUUID, ok := params["cluster_uuid"].(string)
	if !ok || clusterUUID == "" {
		return sdk.Outcome{Status: sdk.StatusFailed, Error: fmt.Errorf("cluster_uuid is required")}
	}
	dryRun := true
	if value, exists := params["dry_run"]; exists {
		dryRun, ok = value.(bool)
		if !ok {
			return sdk.Outcome{Status: sdk.StatusFailed, Error: fmt.Errorf("dry_run must be a boolean")}
		}
	}

	// Add cluster-specific logic here. This example makes no external changes.
	fmt.Printf("start cluster_uuid=%s\n", clusterUUID)
	fmt.Printf("processing cluster_uuid=%s dry_run=%t\n", clusterUUID, dryRun)
	fmt.Printf("finished cluster_uuid=%s\n", clusterUUID)
	return sdk.Outcome{
		Status: sdk.StatusSucceeded,
		Data:   map[string]any{"cluster_uuid": clusterUUID, "dry_run": dryRun},
	}
}

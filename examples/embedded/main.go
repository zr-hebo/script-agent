// This example uses its own binary as the Go helper; no standalone HTTP server
// or separate script-agent executable is required.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	agent "github.com/zr-hebo/script-agent"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if handled, code := agent.HandleHelperCommand(ctx, os.Args[1:]); handled {
		os.Exit(code)
	}
	// Start business services / load credentials only after the helper branch.
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	executor, err := agent.New(agent.Config{GoExecutable: executable})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	result := executor.Execute(ctx, agent.Request{
		TaskID: "embedded-example", Language: "go",
		Source: `package usercode
import "context"
func Handle(ctx context.Context, params map[string]any) (map[string]any, error) {
    if err := ctx.Err(); err != nil { return nil, err }
    return params, nil
}`,
		Params: map[string]any{"cluster_uuid": "cluster-001", "dry_run": true},
	})
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if result.Status != "succeeded" || result.Callback.Status == "failed" {
		os.Exit(1)
	}
}

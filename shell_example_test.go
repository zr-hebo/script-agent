package scriptagent_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	agent "github.com/zr-hebo/script-agent"
)

func TestCLICustomShellExample(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{"parameters", map[string]any{"cluster_uuid": "cluster-002", "dry_run": true}},
		{"empty map", map[string]any{}},
		{"quoted text", map[string]any{"cluster_uuid": "中文 'quotes' \"double\" $(echo unexpected)\n", "dry_run": false}},
		{"nested map", map[string]any{"nested": map[string]any{"items": []any{1, "two", false}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params, err := json.Marshal(tc.params)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(helperPath, "run", "--source", "examples/custom/example.sh",
				"--language", "shell", "--bash", "/bin/bash", "--params", string(params))
			// No external commands are available: the example must use Bash only.
			cmd.Env = []string{"PATH=" + t.TempDir()}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("%v, output=%s, stderr=%s", err, output, stderr.String())
			}
			var result agent.Result
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatalf("invalid result JSON: %v, %s", err, output)
			}
			cluster, _ := tc.params["cluster_uuid"].(string)
			dryRun, exists := tc.params["dry_run"]
			if !exists {
				dryRun = true
			}
			want := fmt.Sprintf("cluster_uuid=%s\ndry_run=%v\n", cluster, dryRun)
			if result.Status != "succeeded" || result.Outcome.Data != nil || result.Outcome.Stdout != want || result.Outcome.Stderr != "" || stderr.String() != want {
				t.Fatalf("unexpected result/live logs: %s, stderr=%s", output, stderr.String())
			}
			logs := result.Phases[1].Logs
			lines := strings.Split(strings.TrimSuffix(want, "\n"), "\n")
			if len(logs) != len(lines) {
				t.Fatalf("unexpected run logs: %+v", logs)
			}
			for i, line := range lines {
				if logs[i].Stream != "stdout" || logs[i].Message != line {
					t.Fatalf("unexpected run logs: %+v", logs)
				}
			}
			if len(result.Phases[0].Logs) != 0 || len(result.Phases[2].Logs) != 0 || result.Phases[2].Status != "succeeded" {
				t.Fatalf("unexpected default hooks: %+v", result.Phases)
			}
		})
	}
}

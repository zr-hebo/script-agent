package scriptagent_test

import (
	"context"
	"strings"
	"testing"

	agent "github.com/zr-hebo/script-agent"
)

func TestShellLifecycleLineEndings(t *testing.T) {
	for _, format := range []string{"LF", "CRLF", "mixed"} {
		t.Run(format, func(t *testing.T) {
			encode := func(source string) string {
				switch format {
				case "CRLF":
					return strings.ReplaceAll(source, "\n", "\r\n")
				case "mixed":
					return strings.Replace(source, "\n", "\r\n", 2)
				default:
					return source
				}
			}
			const header = "#!/usr/bin/env bash\nset -Eeuo pipefail\n\n"
			out := executor(t, agent.CallbackConfig{}).Execute(context.Background(), agent.Request{
				Language:      "shell",
				PrepareSource: encode(header + "echo ready > prepared.txt\necho prepare\n"),
				Source:        encode(header + "test -f prepared.txt\necho \"cluster_uuid=${PARAM_CLUSTER_UUID:-} dry_run=${PARAM_DRY_RUN:-true}\"\n"),
				PostRunSource: encode(header + "test -s \"$SCRIPT_OUTCOME_FILE\"\necho post-run\n"),
			})
			if out.Status != "succeeded" || out.Outcome.Stderr != "" {
				t.Fatalf("status=%s error=%v stderr=%q", out.Status, out.Outcome.Error, out.Outcome.Stderr)
			}
			if want := "prepare\ncluster_uuid= dry_run=true\npost-run\n"; out.Outcome.Stdout != want {
				t.Fatalf("stdout=%q want=%q", out.Outcome.Stdout, want)
			}
			if len(out.Phases) != 3 {
				t.Fatalf("phases=%+v", out.Phases)
			}
			for _, phase := range out.Phases {
				if phase.Status != "succeeded" || len(phase.Logs) != 1 {
					t.Fatalf("phase=%+v", phase)
				}
			}
		})
	}
}

func TestShellCRLFPreservesLoneCRAndParameterValues(t *testing.T) {
	out := executor(t, agent.CallbackConfig{}).Execute(context.Background(), agent.Request{
		Language: "shell",
		Source:   "printf '%s' 'left\rright'\r\nprintf '%s' \"$PARAM_TEXT\"\r\n",
		Params:   map[string]any{"text": "param\r\nvalue"},
	})
	if out.Status != "succeeded" || out.Outcome.Stdout != "left\rrightparam\r\nvalue" {
		t.Fatalf("status=%s stdout=%q stderr=%q", out.Status, out.Outcome.Stdout, out.Outcome.Stderr)
	}
}

func TestShellCRLFFailureStillRunsPostRun(t *testing.T) {
	out := executor(t, agent.CallbackConfig{}).Execute(context.Background(), agent.Request{
		Language:      "shell",
		Source:        "#!/usr/bin/env bash\r\nset -Eeuo pipefail\r\nexit 7\r\n",
		PostRunSource: "set -Eeuo pipefail\r\necho cleanup\r\n",
	})
	if out.Status != "failed" || out.Outcome.ExitCode == nil || *out.Outcome.ExitCode != 7 || out.Outcome.Error == nil {
		t.Fatalf("outcome=%+v", out.Outcome)
	}
	if len(out.Phases) != 3 || out.Phases[1].Status != "failed" || out.Phases[2].Status != "succeeded" || !strings.Contains(out.Outcome.Stdout, "cleanup\n") {
		t.Fatalf("phases=%+v stdout=%q", out.Phases, out.Outcome.Stdout)
	}
}

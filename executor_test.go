package scriptagent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/zr-hebo/script-agent"
)

var helperPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "script-agent-test-build-")
	if err != nil {
		panic(err)
	}
	helperPath = filepath.Join(dir, "script-agent")
	cmd := exec.Command("go", "build", "-o", helperPath, "./cmd/script-agent")
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, string(output), err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func executor(t *testing.T, callback agent.CallbackConfig) *agent.Executor {
	t.Helper()
	e, err := agent.New(agent.Config{GoExecutable: helperPath, GracePeriod: 50 * time.Millisecond, Callback: callback})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

const goSuccess = `package usercode
import ("context"; "fmt")
func Handle(ctx context.Context, params map[string]interface{}) (map[string]interface{}, error) {
    if err := ctx.Err(); err != nil { return nil, err }
    fmt.Println("go log")
    return params, nil
}`

func TestLanguagesAndMapParameters(t *testing.T) {
	e := executor(t, agent.CallbackConfig{})
	for _, tc := range []struct{ language, source, log string }{
		{"go", goSuccess, "go log"},
		{"python", "def handle(params):\n    print('python log')\n    return params\n", "python log"},
		{"shell", `python3 - <<'PY'
import json, os
with open(os.environ["SCRIPT_PARAMS_FILE"]) as f:
    params = json.load(f)
assert os.environ["BATCH_PARAMS_FILE"] == os.environ["SCRIPT_PARAMS_FILE"]
with open(os.environ["SCRIPT_RESULT_FILE"], "w") as f:
    json.dump(params, f)
print("shell log")
PY`, "shell log"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			params := map[string]any{"cluster_uuid": "cluster-001", "dry_run": false, "count": 3, "nested": map[string]any{"name": "空 格 \"quotes\" $(touch should-not-exist)"}, "items": []any{1, "two"}}
			var phases []string
			result := e.ExecuteWithObserver(context.Background(), "test-id", agent.Request{TaskID: "task-001", Language: tc.language, Source: tc.source, Params: params}, func(phase string) { phases = append(phases, phase) })
			if result.Status != "succeeded" {
				t.Fatalf("%+v, stderr=%s", result, result.Outcome.Stderr)
			}
			want, _ := json.Marshal(params)
			got, _ := json.Marshal(result.Outcome.Data)
			if string(got) != string(want) {
				t.Fatalf("params: got %s want %s", got, want)
			}
			if !strings.Contains(result.Outcome.Stdout, tc.log) {
				t.Fatal(result.Outcome.Stdout)
			}
			if strings.Join(phases, ",") != "prepare,run,post-run" {
				t.Fatal(phases)
			}
			if result.Outcome.ExitCode == nil || *result.Outcome.ExitCode != 0 {
				t.Fatal(result.Outcome.ExitCode)
			}
			for _, phase := range result.Phases {
				if phase.Status != "succeeded" || phase.StartedAt == nil || phase.FinishedAt == nil {
					t.Fatal(phase)
				}
			}
		})
	}
}

func TestScriptFailures(t *testing.T) {
	e := executor(t, agent.CallbackConfig{})
	for _, tc := range []struct{ name, language, source, contains string }{
		{"go syntax", "go", "package usercode\nfunc broken(", ""},
		{"go package", "go", "package main\nfunc main() {}", "package usercode"},
		{"go signature", "go", "package usercode\nfunc Handle() {}", "expected Handle"},
		{"go error", "go", `package usercode
import ("context"; "fmt")
func Handle(ctx context.Context, p map[string]interface{}) (map[string]interface{}, error) { return nil, fmt.Errorf("business failure") }`, "business failure"},
		{"go panic", "go", `package usercode
import "context"
func Handle(ctx context.Context, p map[string]interface{}) (map[string]interface{}, error) { panic("go boom") }`, "go boom"},
		{"python exception", "python", "def handle(params):\n    raise ValueError('python boom')", "python boom"},
		{"python syntax", "python", "def handle(:", "SyntaxError"},
		{"python signature", "python", "x = 1", "expected def run"},
		{"python result", "python", "def handle(params):\n    return [1, 2]", "dict or None"},
		{"python exit zero without result", "python", "import os\ndef handle(params):\n    os._exit(0)", "missing result"},
		{"shell exit", "shell", "echo shell-boom >&2\nexit 7", "exit status 7"},
		{"shell pipefail", "shell", "false | true\necho should-not-run", "exit status 1"},
		{"shell result", "shell", "printf 'not json' > \"$SCRIPT_RESULT_FILE\"", "result JSON"},
		{"shell fifo", "shell", "mkfifo \"$SCRIPT_RESULT_FILE\"", "regular file"},
		{"shell symlink", "shell", "ln -s \"$SCRIPT_PARAMS_FILE\" \"$SCRIPT_RESULT_FILE\"", "result JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := e.Execute(context.Background(), agent.Request{Language: tc.language, Source: tc.source})
			if result.Status != "failed" || result.Outcome.Error == nil {
				t.Fatalf("%+v", result)
			}
			if !strings.Contains(result.Outcome.Error.Error(), tc.contains) {
				t.Fatalf("%+v, stderr=%s", result.Outcome.Error, result.Outcome.Stderr)
			}
			if result.Phases[2].Status != "succeeded" {
				t.Fatal(result.Phases)
			}
			if strings.Contains(result.Outcome.Stdout, "should-not-run") {
				t.Fatal(result.Outcome.Stdout)
			}
			if tc.name == "go panic" && result.Outcome.Stack == "" {
				t.Fatal("missing Go panic stack")
			}
			if tc.name == "python exception" && !strings.Contains(result.Outcome.Stack, "Traceback") {
				t.Fatal("missing Python traceback")
			}
			if tc.name == "shell pipefail" && !strings.Contains(result.Outcome.Stderr, "line=") {
				t.Fatal("missing shell error line")
			}
		})
	}
}

func TestTimeoutAllLanguages(t *testing.T) {
	e := executor(t, agent.CallbackConfig{})
	for _, tc := range []struct{ language, source string }{
		{"go", `package usercode
import "context"
func Handle(ctx context.Context, p map[string]interface{}) (map[string]interface{}, error) { for {} }`},
		{"shell", "trap '' TERM\nwhile :; do :; done"},
		{"python", "def handle(params):\n    while True:\n        pass"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			start := time.Now()
			result := e.Execute(context.Background(), agent.Request{Language: tc.language, Source: tc.source, TimeoutSeconds: 1})
			if result.Status != "timed_out" {
				t.Fatalf("%+v, stderr=%s", result, result.Outcome.Stderr)
			}
			if time.Since(start) > 4*time.Second {
				t.Fatal("timeout did not stop process promptly")
			}
		})
	}
}

func TestCancellationAndPostRun(t *testing.T) {
	var event agent.CallbackEvent
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		w.WriteHeader(204)
	}))
	defer callback.Close()
	e := executor(t, agent.CallbackConfig{AllowedOrigins: []string{callback.URL}})
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(200*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()
	result := e.Execute(ctx, agent.Request{Language: "shell", Source: "sleep 30", CallbackURL: callback.URL})
	if result.Status != "cancelled" || result.Callback.Status != "succeeded" {
		t.Fatalf("%+v", result)
	}
	if event.Status != "cancelled" || event.Phase != "post-run" {
		t.Fatalf("%+v", event)
	}
}

func TestCallbackRetryAndIndependentFailure(t *testing.T) {
	var attempts atomic.Int32
	var key string
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer callback-secret" {
			t.Error("missing callback auth")
		}
		if key != "" && key != r.Header.Get("Idempotency-Key") {
			t.Error("callback key changed")
		}
		key = r.Header.Get("Idempotency-Key")
		if attempts.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer callback.Close()
	e := executor(t, agent.CallbackConfig{AllowedOrigins: []string{callback.URL}, BearerToken: "callback-secret"})
	result := e.Execute(context.Background(), agent.Request{Language: "shell", Source: "echo executed-once", CallbackURL: callback.URL})
	if result.Status != "succeeded" || result.Callback.Status != "succeeded" || result.Callback.Attempts != 3 {
		t.Fatalf("%+v", result)
	}
	if result.Outcome.Stdout != "executed-once\n" {
		t.Fatal(result.Outcome.Stdout)
	}
	if key != result.ExecutionID {
		t.Fatal(key)
	}
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }))
	defer failed.Close()
	e = executor(t, agent.CallbackConfig{AllowedOrigins: []string{failed.URL}})
	result = e.Execute(context.Background(), agent.Request{Language: "shell", Source: "true", CallbackURL: failed.URL})
	if result.Status != "succeeded" || result.Callback.Status != "failed" || result.Callback.Attempts != 1 || result.Phases[2].Status != "failed" {
		t.Fatalf("%+v", result)
	}
}

func TestPrepareFailureCallbackAndOriginPolicy(t *testing.T) {
	var calls atomic.Int32
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer callback.Close()
	e := executor(t, agent.CallbackConfig{AllowedOrigins: []string{callback.URL}})
	result := e.Execute(context.Background(), agent.Request{Language: "ruby", Source: "ignored", CallbackURL: callback.URL})
	if result.Status != "failed" || result.Phases[1].Status != "skipped" || result.Callback.Status != "succeeded" || calls.Load() != 1 {
		t.Fatalf("%+v", result)
	}
	e = executor(t, agent.CallbackConfig{})
	result = e.Execute(context.Background(), agent.Request{Language: "shell", Source: "true", CallbackURL: callback.URL})
	if result.Status != "failed" || result.Callback.Attempts != 0 || calls.Load() != 1 {
		t.Fatalf("%+v", result)
	}
}

func TestCallbackRedirectBlocked(t *testing.T) {
	var calls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer origin.Close()
	e := executor(t, agent.CallbackConfig{AllowedOrigins: []string{origin.URL}})
	result := e.Execute(context.Background(), agent.Request{Language: "shell", Source: "true", CallbackURL: origin.URL})
	if result.Callback.Status != "failed" || calls.Load() != 0 {
		t.Fatalf("%+v", result)
	}
}

func TestLogLimitsAndSecretEnvironment(t *testing.T) {
	t.Setenv("SCRIPT_AGENT_TOKEN", "supervisor-secret")
	t.Setenv("BASH_ENV", "/should/not/be/loaded")
	e := executor(t, agent.CallbackConfig{})
	result := e.Execute(context.Background(), agent.Request{Language: "python", Source: `import os
def handle(params):
    assert "SCRIPT_AGENT_TOKEN" not in os.environ
    assert "BASH_ENV" not in os.environ
    print("x" * 100000)
    return {"ok": True}
`})
	if result.Status != "succeeded" || !result.Outcome.StdoutTruncated || len(result.Outcome.Stdout) != 64<<10 {
		t.Fatalf("%+v", result)
	}
}

func TestIndependentGoExecutions(t *testing.T) {
	e := executor(t, agent.CallbackConfig{})
	source := `package usercode
import "context"
var count = 0
func Handle(ctx context.Context, p map[string]interface{}) (map[string]interface{}, error) { count++; return map[string]interface{}{"count": count}, nil }`
	for i := 0; i < 2; i++ {
		result := e.Execute(context.Background(), agent.Request{Language: "go", Source: source})
		if result.Status != "succeeded" || result.Outcome.Data["count"] != float64(1) {
			t.Fatalf("%+v", result)
		}
	}
}

func TestCLI(t *testing.T) {
	body, _ := json.Marshal(agent.Request{Language: "go", Source: goSuccess, Params: map[string]any{"cluster_uuid": "cli"}})
	cmd := exec.Command(helperPath, "run")
	cmd.Stdin = strings.NewReader(string(body))
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var result agent.Result
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err, string(output))
	}
	if result.Status != "succeeded" || result.Outcome.Data["cluster_uuid"] != "cli" {
		t.Fatalf("%+v", result)
	}
}

func TestCLICustomSource(t *testing.T) {
	for _, tc := range []struct {
		name, params string
		code         int
	}{
		{"success", `{"cluster_uuid":"cluster-001","dry_run":true}`, 0},
		{"false and spaces", `{"cluster_uuid":"cluster with spaces","dry_run":false}`, 0},
		{"default dry run", `{"cluster_uuid":"cluster-001"}`, 0},
		{"missing cluster", `{}`, 1},
		{"invalid dry run", `{"cluster_uuid":"cluster-001","dry_run":"false"}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(helperPath, "run", "--source", "examples/custom/example.go", "--params", tc.params)
			output, err := cmd.Output()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.code {
				t.Fatalf("unexpected exit: %v, output=%s", err, output)
			}
			var result agent.Result
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatal(err, string(output))
			}
			if tc.code == 0 {
				var params map[string]any
				_ = json.Unmarshal([]byte(tc.params), &params)
				dryRun, exists := params["dry_run"]
				if !exists {
					dryRun = true
				}
				if result.Status != "succeeded" || result.Outcome.Data["cluster_uuid"] != params["cluster_uuid"] || result.Outcome.Data["dry_run"] != dryRun || result.Outcome.Error != nil {
					t.Fatalf("unexpected result: %+v", result)
				}
			} else if result.Status != "failed" || result.Outcome.Error == nil {
				t.Fatalf("expected business failure: %+v", result)
			}
		})
	}
}

func TestCLISourceInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--source", "examples/custom/example.go", "--file", "-"},
		{"run", "--source", ""},
		{"run", "--params", "{}"},
		{"run", "--language", "go"},
		{"serve", "--source", "examples/custom/example.go"},
		{"serve", "--stream-logs=true"},
		{"run", "--source", "missing-script.go"},
		{"run", "--source", "examples/custom/example.go", "--language", "ruby"},
		{"run", "--source", "examples/custom/example.go", "--params", "[]"},
		{"run", "--source", "examples/custom/example.go", "--params", "null"},
		{"run", "--source", "examples/custom/example.go", "--params", "true"},
		{"run", "--source", "examples/custom/example.go", "--params", "{"},
		{"run", "--source", "examples/custom/example.go", "--params", "{} {}"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := exec.Command(helperPath, args...)
			output, err := cmd.CombinedOutput()
			if err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 2 {
				t.Fatalf("expected input error: %v, output=%s", err, output)
			}
		})
	}
}

// readyLog detects a marker even when the pipe splits it across writes.
type readyLog struct {
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func (w *readyLog) Write(p []byte) (int, error) {
	n, err := w.buffer.Write(p)
	if strings.Contains(w.buffer.String(), "run-ready") {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func (w *readyLog) String() string { return w.buffer.String() }

func TestCLIStreamsBeforeTaskCompletes(t *testing.T) {
	for _, tc := range []struct{ language, source, prepare, post string }{
		{"go", `package usercode
import ("context"; "fmt"; "os"; "time"; "github.com/zr-hebo/script-agent/sdk")
type Task struct{}
func New() sdk.TaskRunner { return &Task{} }
func (t *Task) Prepare(ctx context.Context, p map[string]any) error { fmt.Println("prepare-log"); return nil }
func (t *Task) Run(ctx context.Context, p map[string]any) sdk.Outcome {
 fmt.Println("run-ready\nrun-second")
 for { if _, err := os.Stat(p["marker"].(string)); err == nil { break }; time.Sleep(10*time.Millisecond) }
 fmt.Fprintln(os.Stderr, "error-stream-log")
 return sdk.Outcome{Status:sdk.StatusSucceeded}
}
func (t *Task) PostRun(ctx context.Context, p map[string]any, out sdk.Outcome) error { fmt.Println("post-log"); return nil }
`, "", ""},
		{"python", `import os, sys, time
def prepare(params):
    print("prepare-log")
def run(params):
    print("run-ready\nrun-second")
    while not os.path.exists(params["marker"]):
        time.sleep(0.01)
    print("error-stream-log", file=sys.stderr)
    return {"status": "succeeded", "data": None, "error": None}
def post_run(params, outcome):
    print("post-log")
`, "", ""},
		{"shell", `echo run-ready
echo run-second
python3 - <<'PY'
import json, os, time
with open(os.environ['SCRIPT_PARAMS_FILE']) as f:
    params = json.load(f)
while not os.path.exists(params['marker']):
    time.sleep(0.01)
PY
echo error-stream-log >&2`, "echo prepare-log", "echo post-log"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "release")
			body, _ := json.Marshal(agent.Request{Language: tc.language, Source: tc.source, PrepareSource: tc.prepare, PostRunSource: tc.post, Params: map[string]any{"marker": marker}, TimeoutSeconds: 10})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, helperPath, "run")
			var stdout bytes.Buffer
			stderr := &readyLog{ready: make(chan struct{})}
			cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(body), &stdout, stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					// Release the script on assertion failure, too.
					_ = os.WriteFile(marker, nil, 0600)
					_ = cmd.Wait()
				}
			}()
			select {
			case <-stderr.ready:
			case <-time.After(5 * time.Second):
				t.Fatal("no live log while task was still running")
			}
			if err := os.WriteFile(marker, nil, 0600); err != nil {
				t.Fatal(err)
			}
			err := cmd.Wait()
			waited = true
			if err != nil {
				t.Fatalf("%v: %s", err, stderr.String())
			}
			var result agent.Result
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Status != "succeeded" {
				t.Fatalf("invalid final JSON: %s, %v", stdout.String(), err)
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(stdout.Bytes(), &fields)
			if _, exists := fields["user_post_run"]; exists {
				t.Fatal("user_post_run is still exposed")
			}
			for i, messages := range [][]string{{"prepare-log"}, {"run-ready", "run-second", "error-stream-log"}, {"post-log"}} {
				phase := result.Phases[i]
				if len(phase.Logs) != len(messages) || phase.LogsTruncated {
					t.Fatalf("unexpected phase logs: %+v", phase)
				}
				for _, message := range messages {
					found := false
					for _, log := range phase.Logs {
						stream := "stdout"
						if message == "error-stream-log" {
							stream = "stderr"
						}
						found = found || (log.Message == message && log.Stream == stream)
					}
					if !found {
						t.Fatalf("missing %s in %+v", message, phase)
					}
				}
			}
			for _, log := range []string{"prepare-log", "run-ready", "post-log", "error-stream-log"} {
				if !strings.Contains(stderr.String(), log) {
					t.Fatalf("missing %s in %s", log, stderr.String())
				}
			}
		})
	}
}

func TestCLIStreamLogsSwitch(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cmd := exec.Command(helperPath, "run", "--source", "examples/custom/example.go", "--params", `{"cluster_uuid":"cli"}`, fmt.Sprintf("--stream-logs=%t", enabled))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v: %s", err, stderr.String())
		}
		var result agent.Result
		if err := json.Unmarshal(output, &result); err != nil || !strings.Contains(result.Outcome.Stdout, "processing") {
			t.Fatalf("capture changed: %s, %v", output, err)
		}
		if strings.Contains(stderr.String(), "processing") != enabled || (!enabled && stderr.Len() != 0) {
			t.Fatalf("enabled=%t stderr=%s", enabled, stderr.String())
		}
	}
}

func TestCLIStreamLogsBrokenPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	defer w.Close()
	cmd := exec.Command(helperPath, "run", "--source", "examples/custom/example.go", "--params", `{"cluster_uuid":"cli"}`)
	cmd.Stderr = w
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("broken log pipe interrupted task: %v, %s", err, output)
	}
	var result agent.Result
	if err := json.Unmarshal(output, &result); err != nil || result.Status != "succeeded" {
		t.Fatalf("invalid final result: %s, %v", output, err)
	}
}

func TestGoContextCancellation(t *testing.T) {
	e := executor(t, agent.CallbackConfig{})
	marker := filepath.Join(t.TempDir(), "started")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(marker); err == nil {
				ready <- true
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		ready <- false
		cancel()
	}()
	result := e.Execute(ctx, agent.Request{Language: "go", Params: map[string]any{"marker": marker}, Source: `package usercode
import ("context"; "fmt"; "os")
func Handle(ctx context.Context, p map[string]any) (map[string]any, error) {
    if err := os.WriteFile(p["marker"].(string), []byte("ready"), 0600); err != nil { return nil, err }
    <-ctx.Done()
    fmt.Println("context cancelled")
    return nil, ctx.Err()
}`})
	if !<-ready {
		t.Fatal("Go helper did not start")
	}
	if result.Status != "cancelled" || !strings.Contains(result.Outcome.Stdout, "context cancelled") {
		t.Fatalf("%+v", result)
	}
}

func TestProcessGroupCancellation(t *testing.T) {
	e := executor(t, agent.CallbackConfig{})
	dir := t.TempDir()
	readyPath, latePath := filepath.Join(dir, "ready"), filepath.Join(dir, "late")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(readyPath); err == nil {
				ready <- true
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		ready <- false
		cancel()
	}()
	// A TERM-ignoring descendant would leave a marker if only the parent was killed.
	source := fmt.Sprintf("(trap '' TERM; sleep 1; echo leaked > %q) &\necho ready > %q\nwait", latePath, readyPath)
	result := e.Execute(ctx, agent.Request{Language: "shell", Source: source})
	if !<-ready {
		t.Fatal("shell did not start")
	}
	if result.Status != "cancelled" {
		t.Fatalf("%+v", result)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(latePath); !os.IsNotExist(err) {
		t.Fatal("descendant survived cancellation", err)
	}
}

func TestCallbackBudget(t *testing.T) {
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Delay beyond the delivery budget without relying on server disconnect
		// detection while the request body remains unread.
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(204)
	}))
	defer callback.Close()
	e := executor(t, agent.CallbackConfig{AllowedOrigins: []string{callback.URL}, Timeout: 100 * time.Millisecond})
	start := time.Now()
	result := e.Execute(context.Background(), agent.Request{Language: "shell", Source: "true", CallbackURL: callback.URL})
	if result.Status != "succeeded" || result.Callback.Status != "failed" || time.Since(start) > 2*time.Second {
		t.Fatalf("%+v", result)
	}
}

package scriptagent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		{"python signature", "python", "x = 1", "expected def handle"},
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
			if !strings.Contains(result.Outcome.Error.Message, tc.contains) {
				t.Fatalf("%+v, stderr=%s", result.Outcome.Error, result.Outcome.Stderr)
			}
			if result.Phases[2].Status != "succeeded" {
				t.Fatal(result.Phases)
			}
			if strings.Contains(result.Outcome.Stdout, "should-not-run") {
				t.Fatal(result.Outcome.Stdout)
			}
			if tc.name == "go panic" && result.Outcome.Error.Stack == "" {
				t.Fatal("missing Go panic stack")
			}
			if tc.name == "python exception" && !strings.Contains(result.Outcome.Error.Stack, "Traceback") {
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

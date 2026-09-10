package scriptagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agent "github.com/zr-hebo/script-agent"
	"github.com/zr-hebo/script-agent/sdk"
)

const goTask = `package usercode
import (
 "context"
 "fmt"
 "github.com/zr-hebo/script-agent/sdk"
)
type Task struct { sdk.BaseTask; count int }
func New() sdk.TaskRunner { return &Task{} }
func (t *Task) Prepare(ctx context.Context, p map[string]any) error {
 t.count = 42
 p["prepared"] = true
 fmt.Println("prepare")
 return nil
}
func (t *Task) Run(ctx context.Context, p map[string]any) sdk.Outcome {
 fmt.Println("run")
 return sdk.Outcome{Status:sdk.StatusSucceeded, Data:map[string]any{"count":t.count,"prepared":p["prepared"]}}
}
func (t *Task) PostRun(ctx context.Context, p map[string]any, out sdk.Outcome) error {
 if out.Status != sdk.StatusSucceeded || out.Error != nil { return fmt.Errorf("unexpected outcome") }
 if t.count != 42 || ctx.Err() != nil { return fmt.Errorf("state/context lost") }
 out.Data["count"] = 100
 fmt.Println("post-run")
 return nil
}`

func TestGoTaskRunnerLifecycle(t *testing.T) {
	e := executor(t, agent.CallbackConfig{})
	out := e.Execute(context.Background(), agent.Request{Language: "go", Source: goTask})
	if out.Status != "succeeded" || out.UserPostRun.Status != "succeeded" {
		t.Fatalf("%+v, primary=%v post=%v stderr=%s", out, out.Outcome.Error, out.UserPostRun.Error, out.Outcome.Stderr)
	}
	if out.Outcome.Data["count"] != float64(42) || out.Outcome.Data["prepared"] != true {
		t.Fatal(out.Outcome.Data)
	}
	if out.Outcome.Stdout != "prepare\nrun\npost-run\n" {
		t.Fatal(out.Outcome.Stdout)
	}
	for _, phase := range out.Phases {
		if phase.Status != "succeeded" {
			t.Fatal(out.Phases)
		}
	}
}

func TestGoDefaultHooks(t *testing.T) {
	source := `package usercode
import ("context"; "github.com/zr-hebo/script-agent/sdk")
type Task struct { sdk.BaseTask }
func New() sdk.TaskRunner { return &Task{} }
func (t *Task) Run(ctx context.Context, p map[string]any) sdk.Outcome { return sdk.Outcome{Status:sdk.StatusSucceeded,Data:p} }`
	out := executor(t, agent.CallbackConfig{}).Execute(context.Background(), agent.Request{Language: "go", Source: source, Params: map[string]any{"cluster_uuid": "c1"}})
	if out.Status != "succeeded" || out.UserPostRun.Status != "succeeded" {
		t.Fatalf("primary=%v post=%v stderr=%s", out.Outcome.Error, out.UserPostRun.Error, out.Outcome.Stderr)
	}
}

func TestOutcomeJSON(t *testing.T) {
	for _, err := range []error{nil, errors.New("business failure")} {
		out := sdk.Outcome{Status: sdk.StatusSucceeded, Error: err}
		if err != nil {
			out.Status = sdk.StatusFailed
		}
		data, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var fields map[string]any
		if json.Unmarshal(data, &fields) != nil {
			t.Fatal(string(data))
		}
		if err == nil && fields["error"] != nil {
			t.Fatal(fields)
		}
		if err != nil && fields["error"] != "business failure" {
			t.Fatal(fields)
		}
		var decoded sdk.Outcome
		if json.Unmarshal(data, &decoded) != nil {
			t.Fatal(string(data))
		}
		if err != nil && decoded.Error.Error() != err.Error() {
			t.Fatal(decoded.Error)
		}
	}
	var out sdk.Outcome
	if json.Unmarshal([]byte(`{"status":"failed","error":{}}`), &out) == nil {
		t.Fatal("accepted object-valued error")
	}
}

func TestPythonLifecycleAndDefaultHooks(t *testing.T) {
	for _, source := range []string{
		`def run(params):
    return {"status":"succeeded", "data":params, "error":None}
`,
		`def prepare(params):
    params["prepared"] = True
def run(params):
    assert params["prepared"]
    return {"status":"succeeded", "data":params, "error":None}
def post_run(params, outcome):
    assert outcome["status"] == "succeeded"
    outcome["data"]["cluster_uuid"] = "changed"
`} {
		out := executor(t, agent.CallbackConfig{}).Execute(context.Background(), agent.Request{Language: "python", Source: source, Params: map[string]any{"cluster_uuid": "c1"}})
		if out.Status != "succeeded" || out.Outcome.Data["cluster_uuid"] != "c1" || out.UserPostRun.Status == "failed" {
			t.Fatalf("%+v %v %v", out, out.Outcome.Error, out.UserPostRun.Error)
		}
	}
}

func TestShellLifecycle(t *testing.T) {
	out := executor(t, agent.CallbackConfig{}).Execute(context.Background(), agent.Request{
		Language: "shell", PrepareSource: "echo ready > prepared.txt\necho prepare",
		Source: "test -f prepared.txt\necho run",
		PostRunSource: `test -f prepared.txt
python3 - <<'PY'
import json, os
with open(os.environ["SCRIPT_OUTCOME_FILE"]) as f:
    out = json.load(f)
assert out["status"] == "succeeded" and out["error"] is None
PY
echo post-run`,
	})
	if out.Status != "succeeded" || out.UserPostRun.Status != "succeeded" || out.Outcome.Stdout != "prepare\nrun\npost-run\n" {
		t.Fatalf("%+v", out)
	}
}

func TestPostRunFailureStillCallbacks(t *testing.T) {
	events := make(chan agent.CallbackEvent, 3)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event agent.CallbackEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		events <- event
		w.WriteHeader(204)
	}))
	defer callback.Close()
	e := executor(t, agent.CallbackConfig{AllowedOrigins: []string{callback.URL}})
	for _, req := range []agent.Request{
		{Language: "go", Source: strings.Replace(goTask, `fmt.Println("post-run")`, `panic("cleanup failed")`, 1)},
		{Language: "python", Source: "def run(p):\n    return {'status':'succeeded','data':None,'error':None}\ndef post_run(p,out):\n    raise ValueError('cleanup failed')"},
		{Language: "shell", Source: "true", PostRunSource: "exit 9"},
	} {
		req.CallbackURL = callback.URL
		out := e.Execute(context.Background(), req)
		if out.Status != "succeeded" || out.UserPostRun.Status != "failed" || out.Callback.Status != "succeeded" || out.Phases[2].Status != "failed" {
			t.Fatalf("%+v %v %v", out, out.Outcome.Error, out.UserPostRun.Error)
		}
		event := <-events
		if event.Status != "succeeded" || event.UserPostRun.Status != "failed" || event.UserPostRun.Error == nil {
			t.Fatal(event)
		}
	}
}

func TestPostRunTimeoutPreservesRun(t *testing.T) {
	e := executor(t, agent.CallbackConfig{})
	for _, req := range []agent.Request{
		{Language: "go", Source: strings.Replace(goTask, `fmt.Println("post-run")`, `for {}`, 1)},
		{Language: "python", Source: "def run(p):\n    return {'status':'succeeded','data':None,'error':None}\ndef post_run(p,out):\n    while True: pass"},
		{Language: "shell", Source: "true", PostRunSource: "trap '' TERM\nwhile :; do :; done"},
	} {
		req.PostRunTimeoutSeconds = 1
		start := time.Now()
		out := e.Execute(context.Background(), req)
		if out.Status != "succeeded" || out.UserPostRun.Status != "timed_out" || time.Since(start) > 4*time.Second {
			t.Fatalf("%+v %v %v", out, out.Outcome.Error, out.UserPostRun.Error)
		}
	}
}

func TestPrepareFailureSkipsRunAndStillCleansUp(t *testing.T) {
	e := executor(t, agent.CallbackConfig{})
	goSource := strings.Replace(goTask, "t.count = 42", `return fmt.Errorf("prepare failed")`, 1)
	goSource = strings.Replace(goSource, `if out.Status != sdk.StatusSucceeded || out.Error != nil`, `if out.Status != sdk.StatusFailed || out.Error == nil`, 1)
	goSource = strings.Replace(goSource, `if t.count != 42 || ctx.Err() != nil`, `if ctx.Err() != nil`, 1)
	goSource = strings.Replace(goSource, `out.Data["count"] = 100`, `fmt.Println(out.Error.Error())`, 1)
	for _, req := range []agent.Request{
		{Language: "go", Source: goSource},
		{Language: "python", Source: `def prepare(p):
    raise ValueError("prepare failed")
def run(p):
    print("should-not-run")
def post_run(p,out):
    assert out["status"] == "failed"
    assert "prepare failed" in out["error"]
    print("post-run")
`},
		{Language: "shell", PrepareSource: "exit 4", Source: "echo should-not-run", PostRunSource: "echo post-run"},
	} {
		out := e.Execute(context.Background(), req)
		if out.Status != "failed" || out.Phases[0].Status != "failed" || out.Phases[1].Status != "skipped" || out.UserPostRun.Status != "succeeded" {
			t.Fatalf("language=%s phases=%+v error=%v cleanup=%v stderr=%s", req.Language, out.Phases, out.Outcome.Error, out.UserPostRun.Error, out.Outcome.Stderr)
		}
		if !strings.Contains(out.Outcome.Stdout, "post-run") || strings.Contains(out.Outcome.Stdout, "should-not-run") {
			t.Fatal(out.Outcome.Stdout)
		}
	}
}

func TestInvalidOutcomesFailAndReachCleanup(t *testing.T) {
	e := executor(t, agent.CallbackConfig{})
	for _, body := range []string{
		`sdk.Outcome{}`,
		`sdk.Outcome{Status:sdk.StatusSucceeded,Error:fmt.Errorf("contradiction")}`,
		`sdk.Outcome{Status:sdk.StatusFailed}`,
		`sdk.Outcome{Status:sdk.StatusSucceeded,Data:map[string]any{"bad":make(chan int)}}`,
	} {
		source := `package usercode
import ("context"; "fmt"; "github.com/zr-hebo/script-agent/sdk")
type Task struct { sdk.BaseTask }
func New() sdk.TaskRunner { return &Task{} }
func(t *Task) Run(ctx context.Context,p map[string]any) sdk.Outcome { fmt.Println("run"); return ` + body + ` }
func(t *Task) PostRun(ctx context.Context,p map[string]any,out sdk.Outcome) error {
 if out.Status!=sdk.StatusFailed || out.Error==nil { return fmt.Errorf("invalid cleanup outcome") }; return nil
}`
		out := e.Execute(context.Background(), agent.Request{Language: "go", Source: source})
		if out.Status != "failed" || out.UserPostRun.Status != "succeeded" {
			t.Fatalf("%v %v stderr=%s", out.Outcome.Error, out.UserPostRun.Error, out.Outcome.Stderr)
		}
	}
	for _, body := range []string{`{}`, `{"status":"succeeded","error":"bad"}`, `{"status":"failed","error":None}`, `{"status":"skipped","error":None}`, `{"status":"failed","error":{}}`} {
		source := "def run(p):\n    return " + body + "\ndef post_run(p,out):\n    assert out['status']=='failed' and out['error']\n"
		out := e.Execute(context.Background(), agent.Request{Language: "python", Source: source})
		if out.Status != "failed" || out.UserPostRun.Status != "succeeded" {
			t.Fatalf("%v %v", out.Outcome.Error, out.UserPostRun.Error)
		}
	}
}

func TestTimeoutCleanupHasIndependentBudget(t *testing.T) {
	e := executor(t, agent.CallbackConfig{})
	for _, req := range []agent.Request{
		{Language: "go", Source: `package usercode
import ("context"; "fmt"; "time"; "github.com/zr-hebo/script-agent/sdk")
type Task struct{sdk.BaseTask}
func New() sdk.TaskRunner{return &Task{}}
func(t *Task) Run(ctx context.Context,p map[string]any)sdk.Outcome{
 <-ctx.Done(); return sdk.Outcome{Status:sdk.StatusFailed,Error:ctx.Err()}
}
func(t *Task) PostRun(ctx context.Context,p map[string]any,out sdk.Outcome)error{
 if out.Status!=sdk.StatusTimedOut || ctx.Err()!=nil { return fmt.Errorf("wrong outcome/context: %v %v",out.Status,ctx.Err()) }
 time.Sleep(200*time.Millisecond)
 fmt.Println("cleaned")
 return ctx.Err()
}`},
		{Language: "python", Source: `import time
def run(p):
    while True: pass
def post_run(p,out):
    assert out["status"] == "timed_out"
    time.sleep(0.2)
    print("cleaned")
`},
		{Language: "shell", Source: "sleep 30", PostRunSource: "sleep 0.2\necho cleaned"},
	} {
		req.TimeoutSeconds, req.PostRunTimeoutSeconds = 1, 2
		out := e.Execute(context.Background(), req)
		if out.Status != "timed_out" || out.UserPostRun.Status != "succeeded" || !strings.Contains(out.Outcome.Stdout, "cleaned") {
			t.Fatalf("language=%s status=%s error=%v cleanup=%v stderr=%s", req.Language, out.Status, out.Outcome.Error, out.UserPostRun.Error, out.Outcome.Stderr)
		}
	}
}

func TestRunDeadlineDoesNotCancelPostRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := agent.Request{Language: "python", TimeoutSeconds: 1, PostRunTimeoutSeconds: 3, Source: `import time
def run(p):
    return {"status":"succeeded","data":None,"error":None}
def post_run(p,out):
    time.sleep(1.1)
    print("cleaned")
`}
	e := executor(t, agent.CallbackConfig{})
	out := e.ExecuteWithObserver(ctx, "post-cancel", req, func(phase string) {
		if phase == "post-run" {
			cancel()
		}
	})
	if out.Status != "succeeded" || out.UserPostRun.Status != "succeeded" {
		t.Fatalf("%+v", out)
	}
}

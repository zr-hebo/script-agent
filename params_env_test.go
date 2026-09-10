package scriptagent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	agent "github.com/zr-hebo/script-agent"
)

func TestLanguageSpecificParametersAcrossPhases(t *testing.T) {
	const value = "中文 'quotes' $(echo not-code)\nsecond line"
	params := map[string]any{"value": value, "dry_run": false, "count": 3, "items": []any{1, true}, "options": map[string]any{"region": "sg"}, "optional": nil, "empty": "", "PATH": "do-not-use", "HOME": "do-not-use", "SCRIPT_PARAMS_FILE": "do-not-use"}
	goSource := `package usercode
import ("context"; "encoding/json"; "fmt"; "os"; "github.com/zr-hebo/script-agent/sdk")
type Task struct{sdk.BaseTask}
func New() sdk.TaskRunner{return &Task{}}
func check(p map[string]any) error {
 for _,k := range []string{"PARAM_VALUE","PARAM_DRY_RUN","PARAM_COUNT","PARAM_ITEMS","PARAM_OPTIONS","PARAM_OPTIONAL","PARAM_EMPTY","PARAM_PATH"} {
  if _,ok:=os.LookupEnv(k);ok{return fmt.Errorf("unexpected param environment %s",k)}
 }
 if p["dry_run"]!=false || p["count"]!=float64(3) || p["optional"]!=nil || p["nul"]!="\x00" || p["bad-key"]!="value" || p["name"]!="one" || p["NAME"]!="two" {return fmt.Errorf("map value/type changed")}
 if p["options"].(map[string]any)["region"]!="sg" || len(p["items"].([]any))!=2 {return fmt.Errorf("nested values changed")}
 if os.Getenv("PATH")=="do-not-use" || os.Getenv("HOME")=="do-not-use" {return fmt.Errorf("environment changed")}
 data,err:=os.ReadFile(os.Getenv("SCRIPT_PARAMS_FILE"));if err!=nil{return err}
 var original map[string]any;if err=json.Unmarshal(data,&original);err!=nil{return err}
 if original["value"]!=p["value"]{return fmt.Errorf("parameter file changed")};return nil
}
func(t *Task)Prepare(ctx context.Context,p map[string]any)error{fmt.Println("prepare");return check(p)}
func(t *Task)Run(ctx context.Context,p map[string]any)sdk.Outcome{
 fmt.Println("run");if err:=check(p);err!=nil{return sdk.Outcome{Status:sdk.StatusFailed,Error:err}}
 return sdk.Outcome{Status:sdk.StatusSucceeded,Data:p}
}
func(t *Task)PostRun(ctx context.Context,p map[string]any,out sdk.Outcome)error{fmt.Println("post-run");return check(p)}
`
	pythonSource := `import json, os
def check(p):
    for k in ("PARAM_VALUE","PARAM_DRY_RUN","PARAM_COUNT","PARAM_ITEMS","PARAM_OPTIONS","PARAM_OPTIONAL","PARAM_EMPTY","PARAM_PATH"):
        assert k not in os.environ, k
    assert p["dry_run"] is False and p["count"] == 3 and p["optional"] is None
    assert p["nul"] == "\x00" and p["bad-key"] == "value" and p["name"] == "one" and p["NAME"] == "two"
    assert p["options"] == {"region":"sg"} and p["items"] == [1,True]
    assert os.environ["PATH"] != "do-not-use" and os.environ["HOME"] != "do-not-use"
    with open(os.environ["SCRIPT_PARAMS_FILE"]) as f:
        assert json.load(f)["value"] == p["value"]
def prepare(p):
    print("prepare")
    check(p)
def run(p):
    print("run")
    check(p)
    return {"status":"succeeded","data":p,"error":None}
def post_run(p,out):
    print("post-run")
    check(p)
`
	shellCheck := `[[ "$PARAM_DRY_RUN" == false && "$PARAM_COUNT" == 3 ]]
[[ "$PARAM_ITEMS" == '[1,true]' && "$PARAM_OPTIONS" == '{"region":"sg"}' ]]
[[ "$PARAM_OPTIONAL" == null && "${PARAM_EMPTY+x}" == x && "$PARAM_EMPTY" == '' ]]
[[ "$PARAM_PATH" == do-not-use && "$PATH" != do-not-use && "$HOME" != do-not-use ]]
[[ "$PARAM_SCRIPT_PARAMS_FILE" == do-not-use && -f "$SCRIPT_PARAMS_FILE" ]]
[[ "$PARAM_VALUE" == '中文 '\''quotes'\'' $(echo not-code)
second line' ]]
`
	e := executor(t, agent.CallbackConfig{})
	for _, req := range []agent.Request{
		{Language: "go", Source: goSource},
		{Language: "python", Source: pythonSource},
		{Language: "shell", PrepareSource: shellCheck + "echo prepare\nexport PARAM_VALUE=changed", Source: shellCheck + "echo run\nexport PARAM_VALUE=changed", PostRunSource: shellCheck + "echo post-run"},
	} {
		t.Run(req.Language, func(t *testing.T) {
			req.Params = make(map[string]any, len(params))
			for key, value := range params {
				req.Params[key] = value
			}
			if req.Language != "shell" {
				req.Params["bad-key"], req.Params["name"], req.Params["NAME"], req.Params["nul"] = "value", "one", "two", "\x00"
			}
			out := e.Execute(context.Background(), req)
			if out.Status != "succeeded" || out.Phases[2].Status != "succeeded" || out.Outcome.Stdout != "prepare\nrun\npost-run\n" {
				t.Fatalf("unexpected phases/environment: %+v error=%v stderr=%s", out.Phases, out.Outcome.Error, out.Outcome.Stderr)
			}
		})
	}
}

func TestParameterEnvironmentConcurrentIsolation(t *testing.T) {
	t.Setenv("PARAM_CLUSTER_UUID", "supervisor-value")
	e := executor(t, agent.CallbackConfig{})
	done := make(chan error, 8)
	for i := 0; i < cap(done); i++ {
		go func(i int) {
			value := fmt.Sprintf("cluster-%d", i)
			out := e.Execute(context.Background(), agent.Request{Language: "shell", Source: `echo "$PARAM_CLUSTER_UUID"`, Params: map[string]any{"cluster_uuid": value}})
			if out.Status != "succeeded" || strings.TrimSpace(out.Outcome.Stdout) != value {
				done <- fmt.Errorf("expected %s: %+v", value, out)
				return
			}
			done <- nil
		}(i)
	}
	for i := 0; i < cap(done); i++ {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	if os.Getenv("PARAM_CLUSTER_UUID") != "supervisor-value" {
		t.Fatal("supervisor environment was mutated")
	}
	out := e.Execute(context.Background(), agent.Request{Language: "shell", Source: `echo "${PARAM_CLUSTER_UUID-unset}"`})
	if out.Status != "succeeded" || out.Outcome.Stdout != "unset\n" {
		t.Fatal("task inherited supervisor/stale PARAM variable:", out)
	}
}

func TestCLIInvalidParameterEnvironment(t *testing.T) {
	for _, params := range []string{`{"bad-key":"value"}`, `{"name":"one","NAME":"two"}`, `{"value":"secret\u0000value"}`} {
		cmd := exec.Command(helperPath, "run", "--source", "examples/custom/example.sh", "--language", "shell", "--params", params)
		output, err := cmd.Output()
		if err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 1 {
			t.Fatalf("expected invalid request result: %v %s", err, output)
		}
		var out agent.Result
		if err := json.Unmarshal(output, &out); err != nil {
			t.Fatal(err)
		}
		if out.Status != "failed" || out.Phases[0].Status != "failed" || out.Phases[1].Status != "skipped" || out.Outcome.Stdout != "" {
			t.Fatalf("invalid request executed script: %s", output)
		}
		if strings.Contains(string(output), "secret") {
			t.Fatal("parameter value leaked")
		}
	}
}

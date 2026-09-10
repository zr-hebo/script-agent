package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agent "github.com/zr-hebo/script-agent"
)

func testServer(t *testing.T, concurrency, capacity int, callback agent.CallbackConfig) *Server {
	t.Helper()
	e, err := agent.New(agent.Config{GracePeriod: 50 * time.Millisecond, Callback: callback})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(e, "test-token", concurrency, capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func request(s *Server, method, path string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func submitTask(t *testing.T, s *Server, req agent.Request) Execution {
	t.Helper()
	body, _ := json.Marshal(req)
	w := request(s, "POST", "/v1/executions", body)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var item Execution
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	return item
}

func waitFor(t *testing.T, s *Server, id string, condition func(Execution) bool) Execution {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w := request(s, "GET", "/v1/executions/"+id, nil)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var item Execution
		if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
			t.Fatal(err)
		}
		if condition(item) {
			return item
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("execution did not reach expected state")
	return Execution{}
}

func TestSubmitGetAndCancel(t *testing.T) {
	s := testServer(t, 2, 8, agent.CallbackConfig{})
	item := submitTask(t, s, agent.Request{TaskID: "task-001", Language: "shell", Source: "echo hello"})
	finished := waitFor(t, s, item.ExecutionID, func(e Execution) bool { return e.FinishedAt != nil })
	if finished.Status != "succeeded" || finished.Result.Outcome.Stdout != "hello\n" || finished.Result.TaskID != "task-001" {
		t.Fatalf("%+v", finished)
	}
	if w := request(s, "DELETE", "/v1/executions/"+item.ExecutionID, nil); w.Code != 409 {
		t.Fatal(w.Code)
	}
	item = submitTask(t, s, agent.Request{Language: "shell", Source: "sleep 30"})
	waitFor(t, s, item.ExecutionID, func(e Execution) bool { return e.Phase == "run" })
	if w := request(s, "DELETE", "/v1/executions/"+item.ExecutionID, nil); w.Code != 202 {
		t.Fatal(w.Code)
	}
	finished = waitFor(t, s, item.ExecutionID, func(e Execution) bool { return e.FinishedAt != nil })
	if finished.Status != "cancelled" {
		t.Fatalf("%+v", finished)
	}
}

func TestQueuedCancelCapacityAndEviction(t *testing.T) {
	s := testServer(t, 1, 2, agent.CallbackConfig{})
	first := submitTask(t, s, agent.Request{Language: "shell", Source: "sleep 30"})
	waitFor(t, s, first.ExecutionID, func(e Execution) bool { return e.Phase == "run" })
	second := submitTask(t, s, agent.Request{Language: "shell", Source: "echo should-not-execute"})
	body := []byte(`{"language":"shell","source":"true"}`)
	if w := request(s, "POST", "/v1/executions", body); w.Code != 429 {
		t.Fatal(w.Code)
	}
	if w := request(s, "DELETE", "/v1/executions/"+second.ExecutionID, nil); w.Code != 202 {
		t.Fatal(w.Code)
	}
	finished := waitFor(t, s, second.ExecutionID, func(e Execution) bool { return e.FinishedAt != nil })
	if finished.Status != "cancelled" || finished.Result.Outcome.Stdout != "" || finished.Result.Phases[1].Status != "skipped" {
		t.Fatalf("%+v", finished.Result)
	}
	third := submitTask(t, s, agent.Request{Language: "shell", Source: "true"})
	if third.ExecutionID == second.ExecutionID {
		t.Fatal("reused execution ID")
	}
	if w := request(s, "GET", "/v1/executions/"+second.ExecutionID, nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestValidationAndAuth(t *testing.T) {
	s := testServer(t, 1, 2, agent.CallbackConfig{})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/v1/executions", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatal(w.Code)
	}
	for _, body := range []string{
		`null`, `[]`, `{}`, `{"language":"ruby","source":"x"}`,
		`{"language":"shell","source":"true","params":[]}`,
		`{"language":"shell","source":"true","timeout_seconds":-1}`,
		`{"language":"shell","source":"true","timeout_seconds":3601}`,
		`{"language":"shell","source":"true","post_run_timeout_seconds":-1}`,
		`{"language":"shell","source":"true","post_run_timeout_seconds":61}`,
		`{"language":"python","source":"pass","prepare_source":"true"}`,
		`{"language":"shell","source":"true","extra":1}`,
		`{"language":"shell","source":"true"} {}`,
		`{"language":"shell","source":"true","callback_url":"http://127.0.0.1/admin"}`,
	} {
		if w := request(s, "POST", "/v1/executions", []byte(body)); w.Code != 400 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	large, _ := json.Marshal(agent.Request{Language: "shell", Source: strings.Repeat("x", 600<<10)})
	if w := request(s, "POST", "/v1/executions", large); w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestHTTPNewLifecycleProtocol(t *testing.T) {
	s := testServer(t, 1, 2, agent.CallbackConfig{})
	item := submitTask(t, s, agent.Request{Language: "python", PostRunTimeoutSeconds: 1, Source: `def prepare(p):
    p["prepared"] = True
def run(p):
    assert p["prepared"]
    return {"status":"failed", "data":None, "error":"business failure"}
def post_run(p,out):
    assert out["error"] == "business failure"
    raise ValueError("cleanup failure")
`})
	finished := waitFor(t, s, item.ExecutionID, func(e Execution) bool { return e.FinishedAt != nil })
	if finished.Status != "failed" || finished.Result.Outcome.Error.Error() != "business failure" || finished.Result.UserPostRun.Error == nil {
		t.Fatalf("%+v", finished.Result)
	}
	if finished.Result.Phases[0].Status != "succeeded" || finished.Result.Phases[1].Status != "failed" || finished.Result.Phases[2].Status != "failed" {
		t.Fatal(finished.Result.Phases)
	}
}

func TestShutdownCancelsAndReports(t *testing.T) {
	var mu sync.Mutex
	var statuses []string
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event agent.CallbackEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
		}
		mu.Lock()
		statuses = append(statuses, event.Status)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer callback.Close()
	s := testServer(t, 1, 4, agent.CallbackConfig{AllowedOrigins: []string{callback.URL}})
	first := submitTask(t, s, agent.Request{Language: "shell", Source: "sleep 30", CallbackURL: callback.URL})
	waitFor(t, s, first.ExecutionID, func(e Execution) bool { return e.Phase == "run" })
	submitTask(t, s, agent.Request{Language: "shell", Source: "sleep 30", CallbackURL: callback.URL})
	s.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(statuses) != 2 || statuses[0] != "cancelled" || statuses[1] != "cancelled" {
		t.Fatal(statuses)
	}
	if w := request(s, "POST", "/v1/executions", []byte(`{"language":"shell","source":"true"}`)); w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestPostRunVisibleAndNotCancellable(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(204)
	}))
	defer callback.Close()
	defer close(release)
	s := testServer(t, 1, 2, agent.CallbackConfig{AllowedOrigins: []string{callback.URL}, Timeout: time.Second})
	item := submitTask(t, s, agent.Request{Language: "shell", Source: "true", CallbackURL: callback.URL})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("callback not entered")
	}
	current := waitFor(t, s, item.ExecutionID, func(e Execution) bool { return e.Phase == "post-run" })
	if current.FinishedAt != nil {
		t.Fatal("finished before post-run completed")
	}
	if w := request(s, "DELETE", "/v1/executions/"+item.ExecutionID, nil); w.Code != 409 {
		t.Fatal(w.Code)
	}
}

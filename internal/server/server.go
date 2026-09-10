package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	agent "github.com/zr-hebo/script-agent"
)

type Execution struct {
	ExecutionID string        `json:"execution_id"`
	TaskID      string        `json:"task_id"`
	Status      string        `json:"status"`
	Phase       string        `json:"phase"`
	CreatedAt   time.Time     `json:"created_at"`
	FinishedAt  *time.Time    `json:"finished_at"`
	Result      *agent.Result `json:"result"`
}

type entry struct {
	Execution
	cancel context.CancelFunc
}

// Server is an in-memory adapter, not a durable task queue.
type Server struct {
	executor *agent.Executor
	token    string
	mu       sync.Mutex
	entries  map[string]*entry
	slots    chan struct{}
	capacity int
	closed   bool
	wg       sync.WaitGroup
	mux      *http.ServeMux
}

func New(executor *agent.Executor, token string, concurrency, capacity int) (*Server, error) {
	if concurrency < 1 || capacity < concurrency {
		return nil, fmt.Errorf("capacity must be >= concurrency >= 1")
	}
	s := &Server{executor: executor, token: token, entries: map[string]*entry{}, slots: make(chan struct{}, concurrency), capacity: capacity, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.mux.HandleFunc("POST /v1/executions", s.submit)
	s.mux.HandleFunc("GET /v1/executions/{id}", s.get)
	s.mux.HandleFunc("DELETE /v1/executions/{id}", s.cancel)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req agent.Request
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeJSON(w, 400, map[string]string{"error": "expected a single JSON object"})
		return
	}
	req, err := s.executor.Validate(req)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		writeJSON(w, 503, map[string]string{"error": "server shutting down"})
		return
	}
	if len(s.entries) >= s.capacity {
		// Evict only the oldest fully finished execution. Active tasks are never lost.
		var oldest *entry
		for _, item := range s.entries {
			if item.FinishedAt != nil && (oldest == nil || item.CreatedAt.Before(oldest.CreatedAt)) {
				oldest = item
			}
		}
		if oldest == nil {
			s.mu.Unlock()
			writeJSON(w, 429, map[string]string{"error": "execution capacity reached"})
			return
		}
		delete(s.entries, oldest.ExecutionID)
	}
	id := agent.NewExecutionID()
	ctx, cancel := context.WithCancel(context.Background())
	item := &entry{Execution: Execution{ExecutionID: id, TaskID: req.TaskID, Status: "queued", Phase: "pending", CreatedAt: time.Now().UTC()}, cancel: cancel}
	s.entries[id] = item
	snapshot := item.Execution
	s.wg.Add(1)
	s.mu.Unlock()
	go s.execute(ctx, id, req)
	w.Header().Set("Location", "/v1/executions/"+id)
	writeJSON(w, http.StatusAccepted, snapshot)
}

func (s *Server) execute(ctx context.Context, id string, req agent.Request) {
	defer s.wg.Done()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		// Still execute prepare/post-run to report a cancelled queued task.
	}
	result := s.executor.ExecuteWithObserver(ctx, id, req, func(phase string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		item := s.entries[id]
		item.Phase = phase
		if item.Status != "cancelling" {
			item.Status = "running"
		}
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.entries[id]
	item.cancel()
	item.Status, item.Phase, item.Result = result.Status, result.Phase, &result
	now := time.Now().UTC()
	item.FinishedAt = &now
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	item, ok := s.entries[r.PathValue("id")]
	var snapshot Execution
	if ok {
		snapshot = item.Execution
	}
	s.mu.Unlock()
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "execution not found"})
		return
	}
	writeJSON(w, 200, snapshot)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	item, ok := s.entries[r.PathValue("id")]
	if !ok {
		s.mu.Unlock()
		writeJSON(w, 404, map[string]string{"error": "execution not found"})
		return
	}
	if item.FinishedAt != nil || item.Phase == "post-run" {
		s.mu.Unlock()
		writeJSON(w, 409, map[string]string{"error": "script already finished; post-run cannot be cancelled"})
		return
	}
	item.Status = "cancelling"
	item.cancel()
	snapshot := item.Execution
	s.mu.Unlock()
	writeJSON(w, 202, snapshot)
}

// Close stops admissions, cancels queued/running scripts, and waits for post-run.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	for _, item := range s.entries {
		item.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

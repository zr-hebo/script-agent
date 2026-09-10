package main

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogStream(t *testing.T) {
	var dst bytes.Buffer
	s := newLogStream(&dst)
	want := strings.Repeat("x", 128<<10)
	_, _ = s.Write([]byte(want))
	s.Close()
	s.Close()
	if dst.String() != want {
		t.Fatal("stream truncated or corrupted")
	}
}

type blockedLogWriter struct {
	started, release chan struct{}
	once             sync.Once
	output           bytes.Buffer
}

func (w *blockedLogWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return w.output.Write(p)
}

func TestLogStreamSlowConsumer(t *testing.T) {
	dst := &blockedLogWriter{started: make(chan struct{}), release: make(chan struct{})}
	s := newLogStream(dst)
	defer func() { close(dst.release); s.Close(); <-s.done }()
	_, _ = s.Write([]byte("first"))
	<-dst.started
	var producers sync.WaitGroup
	for i := 0; i < 4; i++ {
		producers.Add(1)
		go func() {
			defer producers.Done()
			_, _ = s.Write(bytes.Repeat([]byte("x"), 1<<20))
		}()
	}
	producers.Wait()
	if s.dropped.Load() == 0 || len(s.queue) > 32 {
		t.Fatal("expected bounded queue and dropped logs")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked on output consumer")
	}
}

type failedLogWriter struct{}

func (failedLogWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestLogStreamBrokenConsumer(t *testing.T) {
	s := newLogStream(failedLogWriter{})
	if n, err := s.Write([]byte("log")); err != nil || n != 3 {
		t.Fatalf("stream failure propagated to producer: %d %v", n, err)
	}
	s.Close()
}

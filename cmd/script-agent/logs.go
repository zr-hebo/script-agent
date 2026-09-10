package main

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const logChunkBytes = 32 << 10

// logStream keeps slow or broken CLI stderr from blocking task execution.
// At most 1 MiB is queued, plus the single chunk being written.
type logStream struct {
	queue   chan []byte
	done    chan struct{}
	dropped atomic.Int64
	once    sync.Once
}

func newLogStream(dst io.Writer) *logStream {
	s := &logStream{queue: make(chan []byte, 32), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		failed := false
		reportDropped := func() {
			if n := s.dropped.Swap(0); n > 0 && !failed {
				_, err := fmt.Fprintf(dst, "\n[script-agent] dropped %d live log bytes: output consumer too slow\n", n)
				failed = err != nil
			}
		}
		for chunk := range s.queue {
			reportDropped()
			if !failed {
				n, err := dst.Write(chunk)
				failed = err != nil || n != len(chunk)
			}
		}
		reportDropped()
	}()
	return s
}

// Write is concurrent-safe. Close must only be called after all producers stop.
func (s *logStream) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		size := min(len(p), logChunkBytes)
		chunk := append([]byte(nil), p[:size]...)
		select {
		case s.queue <- chunk:
		default:
			s.dropped.Add(int64(size))
		}
		p = p[size:]
	}
	return n, nil
}

func (s *logStream) Close() {
	s.once.Do(func() {
		close(s.queue)
		// A blocked output device must not keep the CLI alive indefinitely.
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-s.done:
		case <-timer.C:
		}
	})
}

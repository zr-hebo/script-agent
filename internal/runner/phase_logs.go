package runner

import (
	"bytes"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	MaxPhaseLogRecords = 1024
	MaxLogRecordBytes  = 4096
)

type LogEntry struct {
	Stream  string `json:"stream"`
	Message string `json:"message"`
}

type PhaseLogs struct {
	Logs      []LogEntry
	Truncated bool
	bytes     int
}

type phaseLogStore struct {
	mu     sync.Mutex
	phases map[string]*PhaseLogs
}

func newPhaseLogStore() *phaseLogStore {
	s := &phaseLogStore{phases: make(map[string]*PhaseLogs)}
	for _, name := range []string{"prepare", "run", "post-run"} {
		s.phases[name] = &PhaseLogs{Logs: []LogEntry{}}
	}
	return s
}

func (s *phaseLogStore) add(phase, stream string, line []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.phases[phase]
	if p.Truncated {
		return
	}
	if len(p.Logs) >= MaxPhaseLogRecords || p.bytes+len(line) > MaxLogBytes {
		p.Truncated = true
		return
	}
	p.bytes += len(line)
	p.Logs = append(p.Logs, LogEntry{Stream: stream, Message: strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")})
}

// Each output pipe carries its own phase boundary markers. A separate lifecycle
// pipe alone cannot attribute logs reliably: its reads may race stdout/stderr.
type phaseLogWriter struct {
	capture *limitedLog
	store   *phaseLogStore
	stream  string
	phase   string
	markers [][]byte
	pending []byte
	line    []byte
}

func newPhaseLogWriter(capture *limitedLog, store *phaseLogStore, stream, prefix string) *phaseLogWriter {
	w := &phaseLogWriter{capture: capture, store: store, stream: stream, phase: "prepare"}
	if prefix != "" {
		w.markers = [][]byte{[]byte(prefix + "run\x1f"), []byte(prefix + "post-run\x1f")}
	}
	return w
}

func (w *phaseLogWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.pending = append(w.pending, p...)
	for len(w.pending) > 0 {
		index, markerIndex := -1, -1
		for i, marker := range w.markers {
			if at := bytes.Index(w.pending, marker); at >= 0 && (index < 0 || at < index) {
				index, markerIndex = at, i
			}
		}
		if index >= 0 {
			w.emit(w.pending[:index])
			w.setPhase([]string{"run", "post-run"}[markerIndex])
			w.pending = w.pending[index+len(w.markers[markerIndex]):]
			continue
		}
		// Keep only a possible split marker, never an arbitrary tail of user logs.
		keep := 0
		for _, marker := range w.markers {
			for size := min(len(w.pending), len(marker)-1); size > keep; size-- {
				if bytes.Equal(w.pending[len(w.pending)-size:], marker[:size]) {
					keep = size
					break
				}
			}
		}
		w.emit(w.pending[:len(w.pending)-keep])
		w.pending = append(w.pending[:0], w.pending[len(w.pending)-keep:]...)
		break
	}
	return n, nil
}

func (w *phaseLogWriter) emit(p []byte) {
	_, _ = w.capture.Write(p)
	for len(p) > 0 {
		size := min(len(p), MaxLogRecordBytes-len(w.line))
		if newline := bytes.IndexByte(p[:size], '\n'); newline >= 0 {
			size = newline + 1
		}
		w.line = append(w.line, p[:size]...)
		p = p[size:]
		if w.line[len(w.line)-1] == '\n' {
			w.flushLine()
		} else if len(w.line) == MaxLogRecordBytes {
			// Keep an incomplete trailing UTF-8 rune for the next record.
			start := len(w.line) - 1
			for start > 0 && !utf8.RuneStart(w.line[start]) && len(w.line)-start < utf8.UTFMax {
				start--
			}
			if !utf8.FullRune(w.line[start:]) {
				w.store.add(w.phase, w.stream, w.line[:start])
				w.line = append(w.line[:0], w.line[start:]...)
			} else {
				w.flushLine()
			}
		}
	}
}

func (w *phaseLogWriter) flushLine() {
	if len(w.line) > 0 {
		w.store.add(w.phase, w.stream, w.line)
		w.line = w.line[:0]
	}
}
func (w *phaseLogWriter) setPhase(phase string) {
	w.flushLine()
	w.phase = phase
}

func (w *phaseLogWriter) close() {
	w.emit(w.pending)
	w.pending = nil
	w.flushLine()
}

package runner

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPhaseMarkersAndLinesAcrossWrites(t *testing.T) {
	const prefix = "\x1etest-marker:"
	input := "prepare partial" + prefix + "run\x1f" + "first\nsecond\nlast partial" + prefix + "post-run\x1f" + "cleanup\n"
	for _, size := range []int{1, 2, 7, len(input)} {
		var live bytes.Buffer
		capture := &limitedLog{stream: &live}
		store := newPhaseLogStore()
		w := newPhaseLogWriter(capture, store, "stdout", prefix)
		for p := []byte(input); len(p) > 0; {
			n := min(size, len(p))
			_, _ = w.Write(p[:n])
			p = p[n:]
		}
		w.close()
		want := map[string][]LogEntry{
			"prepare":  {{Stream: "stdout", Message: "prepare partial"}},
			"run":      {{Stream: "stdout", Message: "first"}, {Stream: "stdout", Message: "second"}, {Stream: "stdout", Message: "last partial"}},
			"post-run": {{Stream: "stdout", Message: "cleanup"}},
		}
		for phase, logs := range want {
			if !reflect.DeepEqual(store.phases[phase].Logs, logs) {
				t.Fatalf("chunk size=%d phase=%s logs=%+v", size, phase, store.phases[phase])
			}
		}
		if want := "prepare partialfirst\nsecond\nlast partialcleanup\n"; capture.text() != want || live.String() != want {
			t.Fatalf("internal markers leaked: capture=%q live=%q", capture.text(), live.String())
		}
	}
}

func TestPhaseLogLimitsDoNotStopLiveOutput(t *testing.T) {
	for _, input := range []string{strings.Repeat("\n", MaxPhaseLogRecords+10), strings.Repeat("x", MaxLogBytes+10)} {
		var live bytes.Buffer
		store := newPhaseLogStore()
		w := newPhaseLogWriter(&limitedLog{stream: &live}, store, "stderr", "")
		w.setPhase("run")
		_, _ = w.Write([]byte(input))
		w.close()
		p := store.phases["run"]
		if !p.Truncated || p.bytes > MaxLogBytes || len(p.Logs) > MaxPhaseLogRecords {
			t.Fatalf("phase log limits not applied: %+v", p)
		}
		if live.String() != input {
			t.Fatal("phase log cap truncated live output")
		}
		for _, entry := range p.Logs {
			if len(entry.Message) > MaxLogRecordBytes {
				t.Fatal("unbounded log record")
			}
		}
		// A later phase has its own log budget.
		w.setPhase("post-run")
		_, _ = w.Write([]byte("cleanup\n"))
		w.close()
		if len(store.phases["post-run"].Logs) != 1 || store.phases["post-run"].Truncated {
			t.Fatal("run log cap affected cleanup logs")
		}
	}
}

func TestPhaseLogLongUTF8Line(t *testing.T) {
	store := newPhaseLogStore()
	w := newPhaseLogWriter(&limitedLog{}, store, "stdout", "")
	want := strings.Repeat("中文日志", 1000)
	for _, b := range []byte(want) {
		_, _ = w.Write([]byte{b})
	}
	w.close()
	var got strings.Builder
	for _, entry := range store.phases["prepare"].Logs {
		if !utf8.ValidString(entry.Message) {
			t.Fatalf("split UTF-8 rune: %q", entry.Message)
		}
		got.WriteString(entry.Message)
	}
	if got.String() != want {
		t.Fatal("long line corrupted")
	}
}

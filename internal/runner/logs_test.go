package runner

import (
	"bytes"
	"testing"
)

func TestLimitedLogStreamsBeyondCaptureLimit(t *testing.T) {
	var stream bytes.Buffer
	log := &limitedLog{stream: &stream}
	data := bytes.Repeat([]byte("x"), MaxLogBytes+1234)
	for _, chunk := range [][]byte{data, []byte("after limit")} {
		if n, err := log.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("write=%d %v", n, err)
		}
	}
	if len(log.data) != MaxLogBytes || !log.truncated {
		t.Fatal("capture limit changed")
	}
	if stream.String() != string(data)+"after limit" {
		t.Fatal("live stream was truncated with capture")
	}
}

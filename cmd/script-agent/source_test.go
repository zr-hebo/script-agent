package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(path, []byte("example source"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"go", "shell", "python"} {
		req, err := sourceRequest(path, language, `{"nested":{"items":[1,false,"space quote \" $(echo no)"]}}`)
		if err != nil || req.Language != language || req.Source != "example source" || req.Params["nested"] == nil {
			t.Fatalf("request=%+v, error=%v", req, err)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", (256<<10)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceRequest(path, "go", "{}"); err == nil {
		t.Fatal("oversized source accepted")
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	agent "github.com/zr-hebo/script-agent"
)

func sourceRequest(path, language, paramsJSON string) (agent.Request, error) {
	req := agent.Request{Language: language}
	if language != "go" && language != "shell" && language != "python" {
		return req, fmt.Errorf("--language must be go, shell, or python")
	}
	if err := json.Unmarshal([]byte(paramsJSON), &req.Params); err != nil {
		return req, fmt.Errorf("--params must be a JSON object: %w", err)
	}
	if req.Params == nil {
		return req, fmt.Errorf("--params must be a JSON object, not null")
	}
	f, err := os.Open(path)
	if err != nil {
		return req, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if err != nil {
		return req, err
	}
	if len(data) > 256<<10 {
		return req, fmt.Errorf("source exceeds 256 KiB")
	}
	req.Source = string(data)
	return req, nil
}

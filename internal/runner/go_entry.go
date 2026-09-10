package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"runtime/debug"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// GoEntry only runs in the disposable child process, never in the HTTP server.
func GoEntry(ctx context.Context, sourcePath, paramsPath, resultPath, errorPath string) (code int) {
	writeError := func(kind string, err error, stack string) {
		detail := ExecutionError{Type: kind, Message: err.Error(), Stack: stack}
		if data, marshalErr := json.Marshal(detail); marshalErr == nil {
			_ = os.WriteFile(errorPath, data, 0600)
		}
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	defer func() {
		if p := recover(); p != nil {
			writeError("panic", fmt.Errorf("panic: %v", p), string(debug.Stack()))
		}
	}()
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		writeError("load_error", err, "")
		return
	}
	file, err := parser.ParseFile(token.NewFileSet(), sourcePath, source, parser.PackageClauseOnly)
	if err != nil || file.Name.Name != "usercode" {
		writeError("load_error", fmt.Errorf("Go source must declare package usercode: %v", err), "")
		return
	}
	data, err := os.ReadFile(paramsPath)
	if err != nil {
		writeError("load_error", err, "")
		return
	}
	var params map[string]any
	if err = json.Unmarshal(data, &params); err != nil {
		writeError("load_error", err, "")
		return
	}
	i := interp.New(interp.Options{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Env: os.Environ()})
	if err = i.Use(stdlib.Symbols); err != nil {
		writeError("load_error", err, "")
		return
	}
	if _, err = i.EvalWithContext(ctx, string(source)); err != nil {
		writeError("load_error", err, "")
		return
	}
	symbol, err := i.Eval("usercode.Handle")
	if err != nil {
		writeError("invalid_entrypoint", err, "")
		return
	}
	handle, ok := symbol.Interface().(func(context.Context, map[string]any) (map[string]any, error))
	if !ok {
		writeError("invalid_entrypoint", fmt.Errorf("expected Handle(context.Context, map[string]any) (map[string]any, error)"), "")
		return
	}
	result, err := handle(ctx, params)
	if err != nil {
		writeError("script_error", err, "")
		return
	}
	data, err = json.Marshal(result)
	if err == nil && len(data) > MaxResultBytes {
		err = fmt.Errorf("result exceeds %d bytes", MaxResultBytes)
	}
	if err == nil {
		err = os.WriteFile(resultPath, data, 0600)
	}
	if err != nil {
		writeError("result_error", err, "")
	}
	return
}

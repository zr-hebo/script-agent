package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	agent "github.com/zr-hebo/script-agent"
	"github.com/zr-hebo/script-agent/internal/server"
)

func main() { os.Exit(run()) }

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: script-agent serve|run [options]")
		return 2
	}
	if handled, code := agent.HandleHelperCommand(ctx, os.Args[1:]); handled {
		return code
	}
	if os.Args[1] != "serve" && os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "unknown command")
		return 2
	}
	flags := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8080", "HTTP listen address (serve)")
	file := flags.String("file", "-", "request JSON file; - reads stdin (run)")
	concurrency := flags.Int("concurrency", 4, "maximum active executions including post-run")
	capacity := flags.Int("capacity", 128, "maximum retained executions (oldest finished entry is evicted)")
	origins := flags.String("callback-origins", "", "comma-separated allowed callback origins, e.g. https://batch.example.com")
	bash := flags.String("bash", "bash", "Bash interpreter path")
	python := flags.String("python", "python3", "Python 3 interpreter path")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	config := agent.Config{GoExecutable: executable, BashPath: *bash, PythonPath: *python,
		Callback: agent.CallbackConfig{BearerToken: os.Getenv("SCRIPT_AGENT_CALLBACK_TOKEN")}}
	if *origins != "" {
		for _, origin := range strings.Split(*origins, ",") {
			config.Callback.AllowedOrigins = append(config.Callback.AllowedOrigins, strings.TrimSpace(origin))
		}
	}
	executor, err := agent.New(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if os.Args[1] == "run" {
		var input io.Reader = os.Stdin
		if *file != "-" {
			f, err := os.Open(*file)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			defer f.Close()
			input = f
		}
		data, err := io.ReadAll(io.LimitReader(input, (512<<10)+1))
		if err != nil || len(data) > 512<<10 {
			fmt.Fprintln(os.Stderr, "request unreadable or exceeds 512 KiB")
			return 2
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		var req agent.Request
		if err := decoder.Decode(&req); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			fmt.Fprintln(os.Stderr, "expected a single JSON object")
			return 2
		}
		result := executor.Execute(ctx, req)
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if result.Status != "succeeded" || result.Callback.Status == "failed" {
			return 1
		}
		return 0
	}
	token := os.Getenv("SCRIPT_AGENT_TOKEN")
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) && token == "" {
		fmt.Fprintln(os.Stderr, "SCRIPT_AGENT_TOKEN is required when listening outside loopback")
		return 2
	}
	handler, err := server.New(executor, token, *concurrency, *capacity)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer handler.Close()
	httpServer := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- httpServer.ListenAndServe() }()
	fmt.Fprintln(os.Stderr, "script-agent listening on", *listen, "(not a security sandbox)")
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}
	return 0
}

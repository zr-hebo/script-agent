package scriptagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type CallbackConfig struct {
	// Exact scheme://host[:port] origins. Empty disables all HTTP callbacks.
	AllowedOrigins []string
	// Optional credential belongs to the supervisor and is never passed to scripts.
	BearerToken string
	Timeout     time.Duration // callback-only budget, after user PostRun; default 10 seconds
	Attempts    int           // bounded delivery attempts; default 3, maximum 5
}

type CallbackResult struct {
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	HTTPStatus int    `json:"http_status"`
	Error      string `json:"error,omitempty"`
}

type CallbackEvent struct {
	Event       string  `json:"event"`
	TaskID      string  `json:"task_id"`
	ExecutionID string  `json:"execution_id"`
	Phase       string  `json:"phase"`
	Status      string  `json:"status"`
	Outcome     Outcome `json:"outcome"`
	UserPostRun Outcome `json:"user_post_run"`
}

type callbackSender struct {
	config  CallbackConfig
	origins map[string]bool
	client  *http.Client
}

func newCallbackSender(config CallbackConfig) (*callbackSender, error) {
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	if config.Attempts == 0 {
		config.Attempts = 3
	}
	if config.Timeout < 0 || config.Timeout > time.Minute {
		return nil, fmt.Errorf("callback timeout must be > 0 and <= 1 minute")
	}
	if config.Attempts < 1 || config.Attempts > 5 {
		return nil, fmt.Errorf("callback attempts must be 1..5")
	}
	s := &callbackSender{config: config, origins: map[string]bool{}, client: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	for _, origin := range config.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("invalid callback origin %q", origin)
		}
		s.origins[u.Scheme+"://"+strings.ToLower(u.Host)] = true
	}
	return s, nil
}

func (s *callbackSender) validateURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > 2048 {
		return fmt.Errorf("callback URL exceeds 2048 bytes")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || !s.origins[u.Scheme+"://"+strings.ToLower(u.Host)] {
		return fmt.Errorf("callback URL must use an explicitly allowed origin, without credentials or fragment")
	}
	return nil
}

func (s *callbackSender) send(target string, event CallbackEvent) CallbackResult {
	result := CallbackResult{Status: "failed"}
	if err := s.validateURL(target); err != nil {
		result.Error = err.Error()
		return result
	}
	body, err := json.Marshal(event)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
	defer cancel()
	for attempt := 1; attempt <= s.config.Attempts; attempt++ {
		if ctx.Err() != nil {
			result.Error = ctx.Err().Error()
			break
		}
		result.Attempts = attempt
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			result.Error = err.Error()
			break
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", event.ExecutionID)
		if s.config.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+s.config.BearerToken)
		}
		resp, err := s.client.Do(req)
		retry := true
		if err != nil {
			// Avoid leaking URL query strings or credentials through transport errors.
			result.Error = "callback transport failed"
		} else {
			result.HTTPStatus = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				result.Status, result.Error = "succeeded", ""
				return result
			}
			result.Error = fmt.Sprintf("callback returned HTTP %d", resp.StatusCode)
			retry = resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500
		}
		if !retry || attempt == s.config.Attempts {
			break
		}
		timer := time.NewTimer(time.Duration(attempt) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return result
}

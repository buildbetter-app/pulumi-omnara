package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultAPIURL = "https://api.omnara.com/v1"

// client calls the Omnara REST API.
type client struct {
	base      string
	token     string
	userAgent string
	http      *http.Client
	// sleep is replaced in tests.
	sleep func(context.Context, time.Duration) error
}

func newClient(base, token, userAgent string) *client {
	return &client{
		base:      strings.TrimRight(base, "/"),
		token:     token,
		userAgent: userAgent,
		http:      &http.Client{Timeout: 2 * time.Minute},
		sleep:     sleepCtx,
	}
}

// request is one API call.
type request struct {
	method      string
	path        string
	query       url.Values
	body        []byte
	contentType string
	// idempotent requests carry an Idempotency-Key and may be retried.
	idempotent bool
}

// apiError is a non-2xx response.
type apiError struct {
	Method  string
	Path    string
	Status  int
	Code    string
	Message string
	Issues  []json.RawMessage
}

func (e *apiError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "omnara %s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Code != "" {
		fmt.Fprintf(&b, " %s", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	for _, issue := range e.Issues {
		fmt.Fprintf(&b, "\n  - %s", issue)
	}
	return b.String()
}

func isStatus(err error, codes ...int) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.Status == c {
			return true
		}
	}
	return false
}

const maxAttempts = 5

// do sends a request, retrying transient failures, and returns the body of
// a 2xx response.
func (c *client) do(ctx context.Context, r request) ([]byte, error) {
	if c.token == "" {
		return nil, errors.New("omnara: no API key configured; set omnara:apiKey or OMNARA_API_KEY")
	}
	u := c.base + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	key := ""
	if r.idempotent {
		key = newIdempotencyKey()
	}
	// Reads, PUTs, DELETEs, and keyed POSTs are safe to retry.
	retryable := r.method == http.MethodGet || r.method == http.MethodPut || r.method == http.MethodDelete || key != ""
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, r.method, u, bytes.NewReader(r.body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)
		if r.body != nil {
			req.Header.Set("Content-Type", r.contentType)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("omnara %s %s: %w", r.method, r.path, err)
			if retryable && ctx.Err() == nil && attempt < maxAttempts {
				if err := c.sleep(ctx, backoff(attempt, "")); err != nil {
					return nil, err
				}
				continue
			}
			return nil, lastErr
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("omnara %s %s: read response: %w", r.method, r.path, err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return body, nil
		}
		lastErr = decodeError(r, resp.StatusCode, body)
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusBadGateway ||
			resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout
		// Rate limits never applied the request, so any method may retry.
		if transient && (retryable || resp.StatusCode == http.StatusTooManyRequests) && attempt < maxAttempts {
			if err := c.sleep(ctx, backoff(attempt, resp.Header.Get("Retry-After"))); err != nil {
				return nil, err
			}
			continue
		}
		return nil, lastErr
	}
	return nil, lastErr
}

func decodeError(r request, status int, body []byte) error {
	e := &apiError{Method: r.method, Path: r.path, Status: status}
	var payload struct {
		Error  string            `json:"error"`
		Code   string            `json:"code"`
		Issues []json.RawMessage `json:"issues"`
	}
	if json.Unmarshal(body, &payload) == nil {
		e.Code, e.Message, e.Issues = payload.Code, payload.Error, payload.Issues
	} else {
		e.Message = strings.TrimSpace(string(body))
		if len(e.Message) > 500 {
			e.Message = e.Message[:500] + "…"
		}
	}
	return e
}

func backoff(attempt int, retryAfter string) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, time.Minute)
	}
	return time.Duration(1<<(attempt-1)) * 500 * time.Millisecond
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func newIdempotencyKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "pulumi-" + hex.EncodeToString(b)
}

package common

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client makes service-to-service HTTP calls. It merges the downstream
// X-Trace into the caller's trace and retries once on a connection error
// (the window before Kubernetes removes a dead pod from endpoints).
type Client struct {
	Base string // e.g. http://quiz:8080
	HTTP *http.Client
}

// NewClient returns a Client with a short timeout suited to in-cluster calls.
func NewClient(base string) *Client {
	return &Client{Base: base, HTTP: &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     30 * time.Second,
		},
	}}
}

// ErrUnavailable means the downstream service could not be reached.
var ErrUnavailable = errors.New("service unavailable")

// HTTPError is a non-2xx response from a downstream service.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("downstream %d: %s", e.Status, e.Body) }

// Do sends req (path relative to Base), copying the given headers. out may be nil.
// Returns ErrUnavailable (wrapped) on connection failure after one retry,
// *HTTPError on non-2xx.
func (c *Client) Do(ctx context.Context, method, path string, headers http.Header, body any, out any) (*http.Response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		req, rerr := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(payload))
		if rerr != nil {
			return nil, rerr
		}
		for k, vs := range headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err = c.HTTP.Do(req)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, err
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	FromContext(ctx).AddHeader(resp.Header.Get(HeaderTrace))
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return resp, &HTTPError{Status: resp.StatusCode, Body: string(data)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

// GetJSON is a shorthand for Do(ctx, GET, path, headers, nil, out).
func (c *Client) GetJSON(ctx context.Context, path string, headers http.Header, out any) error {
	_, err := c.Do(ctx, http.MethodGet, path, headers, nil, out)
	return err
}

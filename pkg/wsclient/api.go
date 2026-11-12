// Package wsclient is a client of tstermd's HTTP API and WebSocket viewer
// protocol, as the web UI uses them, for GUI clients: it lists, creates,
// and manages sessions; streams a session's screen; tracks the screen's
// state and its history; and encodes keys and mouse events for programs.
// It has no UI of its own.
package wsclient

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bradfitz/tsterm/pkg/proto"
)

// DefaultPort is tstermd's default HTTP port.
const DefaultPort = "2200"

// A Client talks to one tstermd.
type Client struct {
	// Base is tstermd's base URL, as "http://host:2200".
	Base string

	// HTTP is the HTTP client to use, or nil for http.DefaultClient.
	HTTP *http.Client
}

// NewClient returns a Client for the tstermd at host, which may include a
// port (DefaultPort otherwise) or be a URL.
func NewClient(host string) *Client {
	host = strings.TrimSuffix(host, "/")
	if !strings.Contains(host, "://") {
		if !strings.Contains(host, ":") || strings.HasSuffix(host, "]") {
			host += ":" + DefaultPort
		}
		host = "http://" + host
	}
	return &Client{Base: host}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// APIError is an error response from tstermd.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("tstermd: %d %s", e.Status, http.StatusText(e.Status))
}

// do sends a JSON request and decodes a JSON response into out, if it's
// non-nil and there is one.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		e := &APIError{Status: resp.StatusCode}
		var m struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &m) == nil {
			e.Code, e.Message = m.Code, m.Message
		}
		return e
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

func sessionPath(id string) string { return "/v1/sessions/" + url.PathEscape(id) }

// List returns the sessions, live and ended.
func (c *Client) List(ctx context.Context) ([]proto.SessionSummary, error) {
	var out []proto.SessionSummary
	err := c.do(ctx, "GET", "/v1/sessions", nil, &out)
	return out, err
}

// Create starts a session.
func (c *Client) Create(ctx context.Context, req proto.CreateSessionReq) (proto.SessionInfo, error) {
	var out proto.SessionInfo
	err := c.do(ctx, "POST", "/v1/sessions", req, &out)
	return out, err
}

// Rename names the session id, or removes its name if name is empty.
func (c *Client) Rename(ctx context.Context, id, name string) error {
	return c.do(ctx, "PATCH", sessionPath(id), proto.UpdateSessionReq{Name: &name}, nil)
}

// SetClosed moves the session id out of the user's open set, or into it.
func (c *Client) SetClosed(ctx context.Context, id string, closed bool) error {
	return c.do(ctx, "PATCH", sessionPath(id), proto.UpdateSessionReq{Closed: &closed}, nil)
}

// Kill ends the session id, which freezes.
func (c *Client) Kill(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", sessionPath(id), nil, nil)
}

// Purge deletes all that the ended session id kept.
func (c *Client) Purge(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", sessionPath(id)+"?purge=1", nil, nil)
}

// WebURL returns the web UI's URL for the session id.
func (c *Client) WebURL(id string) string { return c.Base + "/s/" + url.PathEscape(id) }

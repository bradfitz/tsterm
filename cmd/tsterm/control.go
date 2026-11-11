package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bradfitz/tsterm/pkg/proto"
)

// control is a thin HTTP/CBOR client for the tstermd control plane.
type control struct {
	base   string // e.g. "http://braid:2200"
	client *http.Client
}

func newControl(host string) *control {
	if !strings.Contains(host, ":") {
		host += ":2200"
	}
	base := "http://" + host
	return &control{
		base:   base,
		client: &http.Client{},
	}
}

// host returns the host portion (no port) of the control base URL. UDP
// attach reuses it to dial the daemon.
func (c *control) host() string {
	rest := strings.TrimPrefix(c.base, "http://")
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		return rest[:i]
	}
	return rest
}

func (c *control) list(ctx context.Context) ([]proto.SessionSummary, error) {
	var out []proto.SessionSummary
	if err := c.do(ctx, "GET", "/v1/sessions?sort=activity", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *control) create(ctx context.Context, req proto.CreateSessionReq) (proto.SessionInfo, error) {
	var out proto.SessionInfo
	err := c.do(ctx, "POST", "/v1/sessions", req, &out)
	return out, err
}

func (c *control) info(ctx context.Context, idOrName string) (proto.SessionInfo, error) {
	var out proto.SessionInfo
	err := c.do(ctx, "GET", "/v1/sessions/"+idOrName, nil, &out)
	return out, err
}

func (c *control) kill(ctx context.Context, idOrName string) error {
	return c.do(ctx, "DELETE", "/v1/sessions/"+idOrName, nil, nil)
}

func (c *control) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := proto.Encode(body)
		if err != nil {
			return fmt.Errorf("encode: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", proto.ContentType)
	}
	req.Header.Set("Accept", proto.ContentType)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var pe proto.Error
		if err := proto.Decode(raw, &pe); err == nil && pe.Code != "" {
			return &httpError{Status: resp.StatusCode, Code: pe.Code, Msg: pe.Message}
		}
		return &httpError{Status: resp.StatusCode, Msg: string(raw)}
	}
	if out != nil && len(raw) > 0 {
		if err := proto.Decode(raw, out); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
	}
	return nil
}

type httpError struct {
	Status int
	Code   string
	Msg    string
}

func (e *httpError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("control: %d %s: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("control: %d %s", e.Status, e.Msg)
}

// isNotFound reports whether err is a 404 from the control plane.
func isNotFound(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

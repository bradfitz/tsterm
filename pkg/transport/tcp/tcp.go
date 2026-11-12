// Package tcp implements the HTTP/CBOR control plane for tstermd.
//
// All routes carry CBOR-encoded bodies (Content-Type application/cbor) and
// are gated by auth.SelfUser.HTTPMiddleware. Routes pivot on a session
// identifier that may be either the session ID or its human-readable name;
// the manager's Resolve handles both.
package tcp

import (
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/bradfitz/tsterm/pkg/auth"
	"github.com/bradfitz/tsterm/pkg/proto"
	"github.com/bradfitz/tsterm/pkg/session"
)

// Server holds the dependencies shared by the control-plane handlers.
//
// WSHandler is injected to keep the WebSocket viewer out of this package's
// import graph. It receives requests routed to /v1/sessions/{id}/ws after
// the auth middleware has already run.
type Server struct {
	Manager *session.Manager

	// Auth decides what each request's peer may do: the owner,
	// everything; a viewer, on the sharing node, look at the sessions
	// the owner shares, read-only (see auth.Access).
	Auth      auth.Authorizer
	UDPPort   int
	WSHandler http.Handler

	// ShareURL, if non-nil, returns the web UI's URL on tstermd's sharing
	// node, which the owner's pages offer to share sessions on. With
	// none, or while it returns "" (as before the node is up), there's
	// no sharing, and they don't.
	ShareURL func() string

	// ShareNode, if non-nil, lets the owner start and destroy the
	// sharing node, at /v1/share.
	ShareNode ShareNodeControl

	// ViewAs, if non-nil, as on the sharing node, lets the owner see
	// what other people see there, and switch back, at /v1/view-as.
	ViewAs ViewAsControl

	// TLS, if non-nil, configures HTTPS, served on the same port as
	// cleartext HTTP (see Serve).
	TLS *tls.Config

	// HTTPSHost, if non-empty, is the host name, typically the node's
	// MagicDNS name, that TLS serves a certificate for. Cleartext requests
	// for the web UI's pages are redirected there, on the same port:
	// browsers allow some features, such as notifications, only over
	// HTTPS. The API isn't redirected; it's already encrypted by
	// WireGuard, and its clients need no certificate.
	HTTPSHost string
}

// Handler returns the assembled control-plane handler with auth applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /s/{id}", s.viewer)
	mux.Handle("GET /static/", http.FileServerFS(webRoot))
	mux.HandleFunc("GET /v1/sessions", s.list)
	// Only the owner may change sessions, or get one's info, which has
	// the token for attaching to it.
	mux.HandleFunc("POST /v1/sessions", ownerOnly(s.create))
	mux.HandleFunc("GET /v1/sessions/{id}", ownerOnly(s.info))
	mux.HandleFunc("DELETE /v1/sessions/{id}", ownerOnly(s.kill))
	mux.HandleFunc("PATCH /v1/sessions/{id}", ownerOnly(s.update))
	if s.ShareNode != nil {
		mux.HandleFunc("GET /v1/share", ownerOnly(s.shareStatus))
		mux.HandleFunc("POST /v1/share", ownerOnly(s.shareStart))
		mux.HandleFunc("DELETE /v1/share", ownerOnly(s.shareDestroy))
	}
	if s.ViewAs != nil {
		mux.HandleFunc("POST /v1/view-as", s.setViewAs)
	}
	if s.WSHandler != nil {
		mux.Handle("GET /v1/sessions/{id}/ws", s.WSHandler) // which checks what peers may do
	}
	return s.redirectToHTTPS(auth.Middleware(s.Auth, mux))
}

// ShareNodeControl starts and destroys tstermd's sharing node.
type ShareNodeControl interface {
	Status(context.Context) proto.ShareNode

	// Start starts the node, if it's off.
	Start(context.Context, proto.StartShareNodeReq) error

	// Destroy stops the node, logs it out, and forgets it, and stops
	// sharing every session, so that a later node shares none of them
	// unasked.
	Destroy(context.Context) error
}

// ViewAsControl switches whether the owner sees what others see (see
// auth.Sharing.SetViewAsOther).
type ViewAsControl interface {
	ViewAsOther() bool
	SetViewAsOther(bool)
}

// ViewAsReq is the body of POST /v1/view-as.
type ViewAsReq struct {
	Other bool `json:"other"` // see what others see; or, false, as the owner
}

// ownerOnly wraps h to refuse any peer but the owner.
func ownerOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth.PeerFrom(r.Context()).Access != auth.Owner {
			writeError(w, r, http.StatusForbidden, "forbidden", "only the owner may do that")
			return
		}
		h(w, r)
	}
}

// visible reports whether the peer of a request with ctx may see the
// session id: the owner sees all, a viewer those shared.
func (s *Server) visible(ctx context.Context, id string) bool {
	switch auth.PeerFrom(ctx).Access {
	case auth.Owner:
		return true
	case auth.Viewer:
		if sess, err := s.Manager.Resolve(id); err == nil {
			return sess.Shared()
		}
		f := s.Manager.Frozen(id)
		return f != nil && f.Shared
	}
	return false
}

// isWebPage reports whether path is one of the web UI's pages or assets,
// as opposed to the API.
func isWebPage(path string) bool {
	return path == "/" || strings.HasPrefix(path, "/s/") || strings.HasPrefix(path, "/static/")
}

// redirectToHTTPS wraps h to redirect cleartext requests for web pages to
// HTTPSHost, if set.
func (s *Server) redirectToHTTPS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || s.HTTPSHost == "" || r.Method != "GET" || !isWebPage(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		u := *r.URL
		u.Scheme = "https"
		u.Host = s.HTTPSHost
		if port := localPort(r.Context()); port != "" && port != "443" {
			u.Host = net.JoinHostPort(s.HTTPSHost, port)
		}
		// Temporary, in case HTTPS is later turned off for the tailnet.
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
}

// shareURL returns s.ShareURL's, if any.
func (s *Server) shareURL() string {
	if s.ShareURL == nil {
		return ""
	}
	return s.ShareURL()
}

// Serve runs the control plane on ln until ctx is canceled. If s.TLS is
// set, it serves both HTTPS and cleartext HTTP on ln, telling them apart by
// each connection's first byte.
func Serve(ctx context.Context, ln net.Listener, s *Server) error {
	if s.TLS != nil {
		ln = newSniffListener(ln, s.TLS)
	}
	srv := &http.Server{Handler: s.Handler()}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	summaries := s.Manager.List() // by creation time
	if auth.PeerFrom(r.Context()).Access != auth.Owner {
		summaries = slices.DeleteFunc(summaries, func(sm session.Summary) bool { return !sm.Shared })
	}
	switch sortBy := r.URL.Query().Get("sort"); sortBy {
	case "", "created":
	case "activity":
		slices.SortStableFunc(summaries, func(a, b session.Summary) int {
			return b.LastActivity().Compare(a.LastActivity())
		})
	case "attach":
		slices.SortStableFunc(summaries, func(a, b session.Summary) int {
			return b.LastAttach.Compare(a.LastAttach)
		})
	default:
		writeError(w, r, http.StatusBadRequest, "bad_sort", `sort must be "created", "activity", or "attach"`)
		return
	}
	wire := make([]proto.SessionSummary, 0, len(summaries))
	for _, sm := range summaries {
		wire = append(wire, proto.SessionSummary{
			ID:           string(sm.ID),
			Name:         sm.Name,
			Rows:         sm.Rows,
			Cols:         sm.Cols,
			Created:      sm.Created,
			Active:       sm.Active,
			Watchers:     sm.Watchers,
			LastInput:    sm.LastInput,
			LastOutput:   sm.LastOutput,
			LastActivity: sm.LastActivity(),
			LastAttach:   sm.LastAttach,
			Ended:        sm.Ended,
			Status:       sm.Status,
			Cmd:          sm.Cmd,
			Closed:       sm.Closed,
			Opened:       sm.Opened,
			Shared:       sm.Shared,
		})
	}
	writeResponse(w, r, http.StatusOK, wire)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "body", err.Error())
		return
	}
	var req proto.CreateSessionReq
	if len(body) > 0 {
		if err := decodeRequest(r, body, &req); err != nil {
			writeError(w, r, http.StatusBadRequest, "decode", err.Error())
			return
		}
	}
	cfg := session.Config{
		Name: req.Name,
		Rows: req.Rows,
		Cols: req.Cols,
	}
	if len(req.Cmd) > 0 {
		cfg.PTY.Command = req.Cmd[0]
		cfg.PTY.Args = req.Cmd[1:]
	}
	if len(req.Env) > 0 {
		cfg.PTY.Env = req.Env
	}
	sess, err := s.Manager.Create(cfg)
	if err != nil {
		if errors.Is(err, session.ErrNameTaken) {
			writeError(w, r, http.StatusConflict, "name_taken", err.Error())
			return
		}
		if errors.Is(err, session.ErrBadName) {
			writeError(w, r, http.StatusBadRequest, "bad_name", err.Error())
			return
		}
		if errors.Is(err, session.ErrNoDemo) {
			writeError(w, r, http.StatusNotFound, "no_demo", err.Error())
			return
		}
		writeError(w, r, http.StatusInternalServerError, "create", err.Error())
		return
	}
	writeResponse(w, r, http.StatusCreated, s.infoFor(sess))
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.resolveOr404(w, r)
	if !ok {
		return
	}
	writeResponse(w, r, http.StatusOK, s.infoFor(sess))
}

// viewAsPage returns what the pages' view-as switch needs, for a request
// with ctx: nil for anyone but the owner (seeing as others or not), and
// where there's no switch.
func (s *Server) viewAsPage(ctx context.Context) map[string]any {
	p := auth.PeerFrom(ctx)
	if s.ViewAs == nil || (p.Access != auth.Owner && !p.AsOther) {
		return nil
	}
	return map[string]any{"Other": p.AsOther}
}

// setViewAs is for the owner, even while they're seeing what others see,
// whom it lets switch back.
func (s *Server) setViewAs(w http.ResponseWriter, r *http.Request) {
	if p := auth.PeerFrom(r.Context()); p.Access != auth.Owner && !p.AsOther {
		writeError(w, r, http.StatusForbidden, "forbidden", "only the owner may do that")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "body", err.Error())
		return
	}
	var req ViewAsReq
	if err := decodeRequest(r, body, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "decode", err.Error())
		return
	}
	s.ViewAs.SetViewAsOther(req.Other)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) shareStatus(w http.ResponseWriter, r *http.Request) {
	writeResponse(w, r, http.StatusOK, s.ShareNode.Status(r.Context()))
}

func (s *Server) shareStart(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "body", err.Error())
		return
	}
	var req proto.StartShareNodeReq
	if len(body) > 0 {
		if err := decodeRequest(r, body, &req); err != nil {
			writeError(w, r, http.StatusBadRequest, "decode", err.Error())
			return
		}
	}
	if err := s.ShareNode.Start(r.Context(), req); err != nil {
		writeError(w, r, http.StatusBadRequest, "share_start", err.Error())
		return
	}
	writeResponse(w, r, http.StatusOK, s.ShareNode.Status(r.Context()))
}

func (s *Server) shareDestroy(w http.ResponseWriter, r *http.Request) {
	if err := s.ShareNode.Destroy(r.Context()); err != nil {
		writeError(w, r, http.StatusInternalServerError, "share_destroy", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "body", err.Error())
		return
	}
	var req proto.UpdateSessionReq
	if err := decodeRequest(r, body, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "decode", err.Error())
		return
	}
	if req.Closed != nil || req.Shared != nil {
		// Live or ended.
		var err error
		if req.Closed != nil {
			err = s.Manager.SetClosed(r.PathValue("id"), *req.Closed)
		}
		if err == nil && req.Shared != nil {
			err = s.Manager.SetShared(r.PathValue("id"), *req.Shared)
		}
		switch {
		case errors.Is(err, session.ErrNotFound):
			writeError(w, r, http.StatusNotFound, "not_found", err.Error())
			return
		case err != nil:
			writeError(w, r, http.StatusInternalServerError, "update", err.Error())
			return
		}
		if req.Name == nil {
			if sess, err := s.Manager.Resolve(r.PathValue("id")); err == nil {
				writeResponse(w, r, http.StatusOK, s.infoFor(sess))
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
	}
	sess, ok := s.resolveOr404(w, r)
	if !ok {
		return
	}
	if req.Name != nil {
		switch err := s.Manager.Rename(string(sess.ID()), *req.Name); {
		case errors.Is(err, session.ErrNameTaken):
			writeError(w, r, http.StatusConflict, "name_taken", err.Error())
			return
		case errors.Is(err, session.ErrBadName):
			writeError(w, r, http.StatusBadRequest, "bad_name", err.Error())
			return
		case err != nil:
			writeError(w, r, http.StatusInternalServerError, "rename", err.Error())
			return
		}
	}
	writeResponse(w, r, http.StatusOK, s.infoFor(sess))
}

// kill ends a live session, which then freezes. With ?purge=1, it also
// deletes all that an ended one kept: its screen, history, and info.
func (s *Server) kill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.Manager.Kill(id)
	if r.URL.Query().Get("purge") == "1" {
		if f := s.Manager.Frozen(id); f != nil || errors.Is(err, session.ErrNotFound) {
			err = s.Manager.Remove(id)
		}
	} else if errors.Is(err, session.ErrNotFound) && s.Manager.Frozen(id) != nil {
		err = nil // already ended
	}
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, r, http.StatusInternalServerError, "kill", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resolveOr404(w http.ResponseWriter, r *http.Request) (*session.Session, bool) {
	sess, err := s.Manager.Resolve(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusNotFound, "not_found", err.Error())
		return nil, false
	}
	return sess, true
}

func (s *Server) infoFor(sess *session.Session) proto.SessionInfo {
	rows, cols := sess.Size()
	return proto.SessionInfo{
		ID:      string(sess.ID()),
		Name:    sess.Name(),
		Token:   sess.Token(),
		UDPPort: s.UDPPort,
		Rows:    rows,
		Cols:    cols,
	}
}

// wantJSON reports whether the client asked for JSON rather than CBOR,
// with an Accept header or ?format=json.
func wantJSON(r *http.Request) bool {
	return r.URL.Query().Get("format") == "json" ||
		strings.Contains(r.Header.Get("Accept"), "application/json")
}

func decodeRequest(r *http.Request, body []byte, v any) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return json.Unmarshal(body, v)
	}
	return proto.Decode(body, v)
}

// writeResponse writes v as CBOR, or as JSON if the client asked for it.
func writeResponse(w http.ResponseWriter, r *http.Request, status int, v any) {
	ctype := proto.ContentType
	var b []byte
	var err error
	if wantJSON(r) {
		ctype = "application/json"
		b, err = json.Marshal(v)
	} else {
		b, err = proto.Encode(v)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	writeResponse(w, r, status, proto.Error{Code: code, Message: msg})
}

package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bradfitz/tsterm/pkg/proto"
)

// ErrAmbiguous is returned by pickSession when the host has more than one
// active session and the caller did not name which to attach.
var ErrAmbiguous = errors.New("multiple sessions present; specify one by name")

// pickResult tells the caller what pickSession decided.
type pickResult struct {
	// AttachID is non-empty when an existing session should be attached.
	AttachID string

	// CreateName is non-empty when a new session should be created with
	// this name (may be a synthesized name or the user's request).
	CreateName string
}

// pickSession encodes the CLI's auto-pick rules for `tsterm HOST [SESSION]`.
//
// With name == "":
//
//   - 0 sessions → create a new auto-named session.
//   - 1 session  → attach to it.
//   - ≥2          → ErrAmbiguous; the caller prints the list.
//
// With name set:
//
//   - if a session has that name → attach to it;
//   - otherwise → caller should error or create depending on -new.
func pickSession(sessions []proto.SessionSummary, name string) (pickResult, error) {
	active := make([]proto.SessionSummary, 0, len(sessions))
	for _, s := range sessions {
		if s.Active {
			active = append(active, s)
		}
	}
	if name != "" {
		for _, s := range active {
			if s.Name == name || s.ID == name {
				return pickResult{AttachID: s.ID}, nil
			}
		}
		return pickResult{}, fmt.Errorf("no session named %q on host", name)
	}
	switch len(active) {
	case 0:
		return pickResult{CreateName: ""}, nil
	case 1:
		return pickResult{AttachID: active[0].ID}, nil
	default:
		return pickResult{}, ErrAmbiguous
	}
}

// formatList renders sessions for the user when the caller needs to print
// them (e.g. on -list or on ambiguous attach).
func formatList(sessions []proto.SessionSummary) string {
	if len(sessions) == 0 {
		return "(no sessions)\n"
	}
	var b strings.Builder
	for _, s := range sessions {
		name := s.Name
		if name == "" {
			name = "-"
		}
		state := "ended"
		if s.Status != "" && s.Status != "exited" {
			state += " (" + s.Status + ")"
		}
		if s.Active {
			state = "active never"
			if !s.LastActivity.IsZero() {
				state = "active " + time.Since(s.LastActivity).Round(time.Second).String() + " ago"
			}
			if s.Closed {
				state += ", tab closed"
			}
		}
		fmt.Fprintf(&b, "%-30s %-12s %-8s %-20s watchers=%d\n",
			s.ID, name, fmt.Sprintf("%dx%d", s.Cols, s.Rows), state, s.Watchers)
	}
	return b.String()
}

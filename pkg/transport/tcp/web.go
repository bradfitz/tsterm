package tcp

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bradfitz/tsterm/pkg/session"
)

// The web UI: a list of sessions and a read-only viewer for each, which
// renders the WebSocket viewer stream. Like every route, these are only
// served to the owner of the local node.

//go:embed web
var webFS embed.FS

var (
	webTmpl = template.Must(template.ParseFS(webFS, "web/*.html"))
	webRoot = must(fs.Sub(webFS, "web")) // serves /static/
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

type indexRow struct {
	ID, Name, Label  string // Label is Name, or the ID's start (see label)
	Age              string // how long ago it started
	Activity, Attach string // how long ago
	Watchers         int
	Closed           bool // its tab is closed
}

// endedRow is a frozen session on the index page.
type endedRow struct {
	ID, Name, Label, Status, Cmd string
	Age, Ended                   string // how long ago
	Closed                       bool
	ended                        time.Time
}

// label names a session in a list: its name, or if it has none, the start
// of its ID, as the viewer's tabs do.
func label(id session.ID, name string) string {
	if name != "" {
		return name
	}
	return string(id)[:min(len(id), len("pty-")+4)]
}

// maxEndedRows bounds the ended sessions the index page lists.
const maxEndedRows = 100

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	summaries := s.Manager.List()
	slices.SortStableFunc(summaries, func(a, b session.Summary) int {
		return b.LastActivity().Compare(a.LastActivity())
	})
	var rows []indexRow
	var ended []endedRow
	for _, sm := range summaries {
		if !sm.Ended.IsZero() || sm.Status != "" {
			ended = append(ended, endedRow{
				ID: string(sm.ID), Name: sm.Name, Label: label(sm.ID, sm.Name), Status: sm.Status, Cmd: strings.Join(sm.Cmd, " "),
				Age: ago(sm.Created), Ended: ago(sm.Ended), Closed: sm.Closed, ended: sm.Ended,
			})
			continue
		}
		if !sm.Active {
			continue
		}
		rows = append(rows, indexRow{
			ID:       string(sm.ID),
			Name:     sm.Name,
			Label:    label(sm.ID, sm.Name),
			Age:      ago(sm.Created),
			Activity: ago(sm.LastActivity()),
			Attach:   ago(sm.LastAttach),
			Watchers: sm.Watchers,
			Closed:   sm.Closed,
		})
	}
	slices.SortStableFunc(ended, func(a, b endedRow) int { return b.ended.Compare(a.ended) })
	more := len(ended) - maxEndedRows
	ended = ended[:min(len(ended), maxEndedRows)]
	s.render(w, "index.html", map[string]any{"Live": rows, "Ended": ended, "MoreEnded": more})
}

// ago describes how long ago t was, or returns "never" for the zero time.
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func (s *Server) viewer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data := map[string]string{}
	if sess, err := s.Manager.Resolve(id); err == nil {
		data["ID"], data["Name"] = string(sess.ID()), sess.Name()
	} else if f := s.Manager.Frozen(id); f != nil {
		data["ID"], data["Name"] = string(f.ID), f.Name
	} else {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	s.render(w, "viewer.html", data)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := webTmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

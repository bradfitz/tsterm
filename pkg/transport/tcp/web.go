package tcp

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"slices"
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
	ID, Name, Size, Age string
	Activity, Attach    string // how long ago
	Watchers            int
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	summaries := s.Manager.List()
	slices.SortStableFunc(summaries, func(a, b session.Summary) int {
		return b.LastActivity().Compare(a.LastActivity())
	})
	var rows []indexRow
	for _, sm := range summaries {
		if !sm.Active {
			continue
		}
		rows = append(rows, indexRow{
			ID:       string(sm.ID),
			Name:     sm.Name,
			Size:     fmt.Sprintf("%d×%d", sm.Cols, sm.Rows),
			Age:      ago(sm.Created),
			Activity: ago(sm.LastActivity()),
			Attach:   ago(sm.LastAttach),
			Watchers: sm.Watchers,
		})
	}
	s.render(w, "index.html", rows)
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
	sess, err := s.Manager.Resolve(r.PathValue("id"))
	if err != nil {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	s.render(w, "viewer.html", map[string]string{
		"ID":   string(sess.ID()),
		"Name": sess.Name(),
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := webTmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

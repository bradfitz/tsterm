package tcp

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"time"
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
	Watchers            int
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	var rows []indexRow
	for _, sm := range s.Manager.List() {
		if !sm.Active {
			continue
		}
		rows = append(rows, indexRow{
			ID:       string(sm.ID),
			Name:     sm.Name,
			Size:     fmt.Sprintf("%d×%d", sm.Cols, sm.Rows),
			Age:      time.Since(sm.Created).Round(time.Second).String(),
			Watchers: sm.Watchers,
		})
	}
	s.render(w, "index.html", rows)
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

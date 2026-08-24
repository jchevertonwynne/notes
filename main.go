// Command notes is a tiny public notes board: a form to add a note, a list
// of existing notes, and a way to delete one. Notes persist as JSON on disk.
// Server-rendered HTML only, standard library only.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"os/signal"

	"notes/internal/metrics"
	"notes/internal/profiling"
	"notes/internal/tracing"
)

// maxNoteLength and maxNotes bound a completely unauthenticated public write
// endpoint. Without a cap here, anyone can grow the store file without limit
// and either fill the disk or make every save (which rewrites the whole
// file) arbitrarily slow.
const (
	maxNoteLength = 1000
	maxNotes      = 500
)

// Note is the persisted and rendered unit. Text is rendered through
// html/template so it is always escaped, never interpolated directly into
// HTML.
type Note struct {
	ID      string    `json:"id"`
	Text    string    `json:"text"`
	Created time.Time `json:"created"`
}

// Store guards the in-memory notes with a mutex because HTTP handlers run
// concurrently, and any read or write of the slice (including the copy made
// for rendering) must happen while holding it.
type Store struct {
	mu    sync.Mutex
	path  string
	notes []Note
}

// NewStore loads notes from path if it exists, or starts empty if it
// doesn't. A missing file is normal on first run, not an error.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read store: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.notes); err != nil {
		return nil, fmt.Errorf("parse store: %w", err)
	}
	return s, nil
}

// List returns a snapshot copy of the notes, newest first, so callers can
// range over it without holding the store's lock.
func (s *Store) List() []Note {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Note, len(s.notes))
	copy(out, s.notes)
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Add validates and appends a note, persisting the result before returning.
// Validation happens both before and (for the count cap) inside the lock:
// the length check is cheap and lock-free, but the count check must be
// atomic with the append or two concurrent requests could both slip in
// under the cap and blow past it.
func (s *Store) Add(ctx context.Context, text string) (Note, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Note{}, errors.New("note text must not be empty")
	}
	if len(text) > maxNoteLength {
		return Note{}, fmt.Errorf("note text exceeds %d characters", maxNoteLength)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.notes) >= maxNotes {
		return Note{}, fmt.Errorf("store is full (limit %d notes)", maxNotes)
	}

	n := Note{ID: newID(), Text: text, Created: time.Now().UTC()}
	s.notes = append(s.notes, n)
	if err := s.saveLocked(ctx); err != nil {
		// Roll back the in-memory append so a failed save doesn't leave
		// memory and disk disagreeing about what was persisted.
		s.notes = s.notes[:len(s.notes)-1]
		return Note{}, fmt.Errorf("save: %w", err)
	}
	return n, nil
}

// Delete removes a note by ID and persists the result. It reports whether a
// note was actually found, so the handler can return 404 rather than
// silently succeeding on an unknown ID.
func (s *Store) Delete(ctx context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := -1
	for i, n := range s.notes {
		if n.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return false, nil
	}

	removed := s.notes[idx]
	s.notes = append(s.notes[:idx], s.notes[idx+1:]...)
	if err := s.saveLocked(ctx); err != nil {
		// Put it back in place; a failed save must not lose data from memory.
		s.notes = append(s.notes, Note{})
		copy(s.notes[idx+1:], s.notes[idx:])
		s.notes[idx] = removed
		return false, fmt.Errorf("save: %w", err)
	}
	return true, nil
}

// saveLocked writes the current notes to disk. Callers must hold s.mu.
//
// It writes to a temp file in the same directory, fsyncs it, and renames it
// over the target instead of writing the target in place. os.Rename within
// a directory is atomic on POSIX filesystems, so any reader (or a process
// that crashes mid-write) sees either the old complete file or the new
// complete file, never a truncated one.
func (s *Store) saveLocked(ctx context.Context) error {
	return withSpan(ctx, "save", func(ctx context.Context) error {
		data, err := json.MarshalIndent(s.notes, "", "  ")
		if err != nil {
			return err
		}

		dir := filepath.Dir(s.path)
		tmp, err := os.CreateTemp(dir, ".notes-*.tmp")
		if err != nil {
			return err
		}
		tmpPath := tmp.Name()
		// If anything below fails before the rename, remove the leftover temp
		// file; once the rename succeeds this is a no-op (the path is gone).
		defer os.Remove(tmpPath)

		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return os.Rename(tmpPath, s.path)
	})
}

// newID generates a short random identifier for a note. Random rather than
// counter-based so it works fine across process restarts without needing to
// persist any sequence state.
func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the system RNG is broken; fall back to
		// a timestamp so note creation still succeeds rather than panics.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// The favicon, embedded rather than served from a directory: this app has no
// other static assets, and a single file does not justify an embed.FS and a
// file server.
//
//go:embed icon.svg
var iconSVG []byte

const pageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Notes</title>
<link rel="icon" type="image/svg+xml" href="/icon.svg">
<style>
  body { font-family: system-ui, sans-serif; max-width: 40rem; margin: 2rem auto; padding: 0 1rem; color: #1a1a1a; }
  h1 { font-size: 1.4rem; }
  form.add { display: flex; gap: 0.5rem; margin-bottom: 1.5rem; }
  textarea { flex: 1; font: inherit; padding: 0.5rem; resize: vertical; min-height: 2.5rem; }
  button { font: inherit; padding: 0.5rem 1rem; cursor: pointer; }
  ul { list-style: none; padding: 0; }
  li { display: flex; justify-content: space-between; align-items: flex-start; gap: 1rem; padding: 0.75rem 0; border-bottom: 1px solid #ddd; }
  .text { white-space: pre-wrap; word-break: break-word; }
  .meta { color: #666; font-size: 0.8rem; }
  form.delete { margin: 0; }
  .error { color: #b00020; margin-bottom: 1rem; }
</style>
</head>
<body>
  <h1>Notes</h1>
  {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
  <form class="add" method="post" action="/notes">
    <textarea name="text" maxlength="{{.MaxLength}}" placeholder="Write a note..." required></textarea>
    <button type="submit">Add</button>
  </form>
  <ul>
    {{range .Notes}}
    <li>
      <div>
        <div class="text">{{.Text}}</div>
        <div class="meta">{{.Created.Format "2006-01-02 15:04:05 UTC"}}</div>
      </div>
      <form class="delete" method="post" action="/notes/{{.ID}}/delete">
        <button type="submit">Delete</button>
      </form>
    </li>
    {{else}}
    <li>No notes yet.</li>
    {{end}}
  </ul>
</body>
</html>
`

type pageData struct {
	Notes     []Note
	MaxLength int
	Error     string
}

// newMux wires up the routes. It's a separate function from main so tests
// can build a handler around a Store without also starting a real server.
func newMux(store *Store) http.Handler {
	tmpl := template.Must(template.New("index").Parse(pageTemplate))

	mux := http.NewServeMux()

	// /healthz must stay cheap and must never touch the store or its file:
	// Kubernetes liveness/readiness probes hit it multiple times a minute
	// for the life of the pod, and it must keep answering even if the
	// notes file or disk is having problems.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /icon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		// Immutable in practice: it changes only when the binary does, and a
		// new binary means a new URL is not needed because browsers revalidate
		// on a new session anyway. A day is a reasonable middle.
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(iconSVG)
	})

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		renderIndex(w, tmpl, store, "")
	})

	mux.HandleFunc("POST /notes", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if _, err := store.Add(r.Context(), r.FormValue("text")); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			renderIndex(w, tmpl, store, err.Error())
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	mux.HandleFunc("POST /notes/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ok, err := store.Delete(r.Context(), id)
		if err != nil {
			http.Error(w, "could not delete note", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "note not found", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	mux.Handle("GET /metrics", metrics.Handler())

	return mux
}

func renderIndex(w http.ResponseWriter, tmpl *template.Template, store *Store, errMsg string) {
	data := pageData{Notes: store.List(), MaxLength: maxNoteLength, Error: errMsg}

	// Rendered into a buffer first. Escaping and buffering solve different
	// problems: html/template guarantees user text is escaped, but writing
	// straight to the ResponseWriter commits a 200 with the first byte, so a
	// template failing halfway sends a truncated page under a success status.
	// Buffering means the response is either the whole page or a clean 500.
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		log.Printf("render index: %v", err)
		http.Error(w, "could not render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(buf.Bytes()); err != nil {
		log.Printf("write index response: %v", err)
	}
}

func main() {
	addr := flag.String("addr", ":8093", "address to listen on")
	storePath := flag.String("store", "notes.json", "path to the notes JSON file")
	otelEndpoint := flag.String("otel-endpoint", "", "host:port of an OTLP/gRPC trace collector; tracing is disabled if empty")
	pprofAddr := flag.String("pprof-addr", ":6060", "listen address for pprof debug endpoints; never expose this outside the cluster")
	flag.Parse()

	go profiling.ListenAndServe(*pprofAddr)

	shutdownTracing, err := tracing.Init(context.Background(), "notes", *otelEndpoint)
	if err != nil {
		log.Fatalf("init tracing: %v", err)
	}

	store, err := NewStore(*storePath)
	if err != nil {
		log.Fatalf("load store: %v", err)
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: tracing.Middleware("notes", metrics.Instrument(newMux(store))),
		// These are set explicitly because this server is exposed to the
		// internet: without them a slow or hostile client can hold a
		// connection open indefinitely and exhaust server resources.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	go func() {
		log.Printf("listening on %s (store: %s)", *addr, *storePath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("shutdown signal received")

	// Kubernetes sends SIGTERM and then, after a grace period, SIGKILL.
	// Shutdown stops accepting new connections and lets in-flight requests
	// finish within this timeout, so we exit cleanly before the grace
	// period runs out and SIGKILL arrives.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if err := shutdownTracing(shutdownCtx); err != nil {
		log.Printf("tracing shutdown: %v", err)
	}
}

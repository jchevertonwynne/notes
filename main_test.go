package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestAddNote(t *testing.T) {
	s := newTestStore(t)

	n, err := s.Add("buy milk")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if n.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if n.Text != "buy milk" {
		t.Fatalf("Text = %q, want %q", n.Text, "buy milk")
	}

	notes := s.List()
	if len(notes) != 1 {
		t.Fatalf("List() len = %d, want 1", len(notes))
	}
	if notes[0].ID != n.ID {
		t.Fatalf("listed note ID = %q, want %q", notes[0].ID, n.ID)
	}
}

func TestDeleteNote(t *testing.T) {
	s := newTestStore(t)

	n, err := s.Add("to be removed")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	ok, err := s.Delete(n.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !ok {
		t.Fatal("Delete returned ok=false for existing note")
	}

	if notes := s.List(); len(notes) != 0 {
		t.Fatalf("List() len = %d after delete, want 0", len(notes))
	}

	// Deleting an already-gone ID should report not-found, not error.
	ok, err = s.Delete(n.ID)
	if err != nil {
		t.Fatalf("Delete of missing note returned error: %v", err)
	}
	if ok {
		t.Fatal("Delete returned ok=true for already-deleted note")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.json")

	s1, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := s1.Add("first"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s1.Add("second"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// A fresh Store built from the same path must see what the first one
	// wrote, proving Add actually persisted rather than only updating memory.
	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reload): %v", err)
	}
	notes := s2.List()
	if len(notes) != 2 {
		t.Fatalf("reloaded List() len = %d, want 2", len(notes))
	}

	texts := map[string]bool{}
	for _, n := range notes {
		texts[n.Text] = true
	}
	if !texts["first"] || !texts["second"] {
		t.Fatalf("reloaded notes missing expected text: %+v", notes)
	}
}

func TestRejectEmptyNote(t *testing.T) {
	s := newTestStore(t)

	for _, text := range []string{"", "   ", "\n\t "} {
		if _, err := s.Add(text); err == nil {
			t.Fatalf("Add(%q) succeeded, want error", text)
		}
	}
	if notes := s.List(); len(notes) != 0 {
		t.Fatalf("List() len = %d, want 0 after rejected adds", len(notes))
	}
}

func TestRejectOverLongNote(t *testing.T) {
	s := newTestStore(t)

	tooLong := strings.Repeat("a", maxNoteLength+1)
	if _, err := s.Add(tooLong); err == nil {
		t.Fatal("Add(over-long note) succeeded, want error")
	}

	ok := strings.Repeat("a", maxNoteLength)
	if _, err := s.Add(ok); err != nil {
		t.Fatalf("Add(note at exactly max length) failed: %v", err)
	}
}

func TestRejectBeyondNoteCap(t *testing.T) {
	s := newTestStore(t)

	for i := 0; i < maxNotes; i++ {
		if _, err := s.Add("note"); err != nil {
			t.Fatalf("Add #%d: %v", i, err)
		}
	}
	if _, err := s.Add("one too many"); err == nil {
		t.Fatal("Add beyond maxNotes succeeded, want error")
	}
	if notes := s.List(); len(notes) != maxNotes {
		t.Fatalf("List() len = %d, want %d", len(notes), maxNotes)
	}
}

// TestHTMLIsEscapedInOutput is the key security test: a note containing
// HTML/JS must render as literal escaped text in the page, never as live
// markup, since this page has no authentication and echoes public input.
func TestHTMLIsEscapedInOutput(t *testing.T) {
	s := newTestStore(t)
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	payload := "<script>alert(1)</script>"
	resp, err := http.PostForm(srv.URL+"/notes", url.Values{"text": {payload}})
	if err != nil {
		t.Fatalf("POST /notes: %v", err)
	}
	resp.Body.Close()

	resp, err = http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(bodyBytes)

	if strings.Contains(page, payload) {
		t.Fatalf("response contains raw unescaped payload:\n%s", page)
	}
	if !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("response does not contain escaped payload:\n%s", page)
	}
}

func TestHealthzDoesNotTouchStore(t *testing.T) {
	// Point at a store file that doesn't exist and can't be created (bad
	// directory), then confirm /healthz still answers ok. If the handler
	// ever touched the store, this would fail loudly.
	s := &Store{path: "/nonexistent/dir/notes.json"}
	srv := httptest.NewServer(newMux(s))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(bodyBytes) != "ok" {
		t.Fatalf("body = %q, want %q", string(bodyBytes), "ok")
	}
}

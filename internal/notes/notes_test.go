package notes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

func put(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sum(body string) string {
	s := sha256.Sum256([]byte(body))
	return hex.EncodeToString(s[:])
}

type logBuffer struct{ bytes.Buffer }

func newPoller(t *testing.T, dirs ...Dir) (*Poller, *attention.Store, *logBuffer) {
	t.Helper()
	store := attention.NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)), attention.StoreConfig{})
	logs := &logBuffer{}
	poller := NewPoller(store, PollerConfig{Interval: time.Hour, Dirs: dirs},
		slog.New(slog.NewTextHandler(logs, nil)))
	return poller, store, logs
}

func TestNewNoteAppearsWithHeadingTitleAndHash(t *testing.T) {
	dir := t.TempDir()
	body := "# Tune the cache\n\nTry a smaller TTL.\n"
	path := put(t, dir, "cache.md", body)
	poller, store, _ := newPoller(t, Dir{Path: dir, Label: "ideas"})

	poller.pollOnce()

	item, ok := store.Get("notes:ideas/cache.md")
	if !ok {
		t.Fatalf("no item for the note; store has %v", store.Items())
	}
	info, _ := os.Stat(path)
	if item.Title != "Tune the cache" || item.Source != "notes" || item.Label != "idea" ||
		item.State != attention.StateNeedsAttention || item.Severity != attention.SeverityInfo {
		t.Errorf("item = %+v", item)
	}
	if item.Context["path"] != path || item.Context["dir"] != dir || item.Context["sha256"] != sum(body) {
		t.Errorf("context = %v", item.Context)
	}
	if !item.UpdatedAt.Equal(info.ModTime()) {
		t.Errorf("updated_at = %s, want mtime %s", item.UpdatedAt, info.ModTime())
	}
	if _, has := item.Context["captured"]; has {
		t.Errorf("captured set without frontmatter: %v", item.Context)
	}
}

func TestNoteWithoutHeadingUsesFileName(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "voice-capture-0042.md", "just words, no heading\n## not a title\n")
	poller, store, _ := newPoller(t, Dir{Path: dir, Label: "ideas"})

	poller.pollOnce()

	item, _ := store.Get("notes:ideas/voice-capture-0042.md")
	if item.Title != "voice-capture-0042" {
		t.Errorf("title = %q", item.Title)
	}
}

func TestEditingANoteChangesHashAndTime(t *testing.T) {
	dir := t.TempDir()
	path := put(t, dir, "a.md", "# One\n")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	poller, store, _ := newPoller(t, Dir{Path: dir, Label: "ideas"})
	poller.pollOnce()
	before, _ := store.Get("notes:ideas/a.md")

	put(t, dir, "a.md", "# Two\nmore\n")
	poller.pollOnce()

	after, _ := store.Get("notes:ideas/a.md")
	if after.Title != "Two" || after.Context["sha256"] == before.Context["sha256"] ||
		after.Context["sha256"] != sum("# Two\nmore\n") || !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("before %v %s, after %v %s", before.Context, before.UpdatedAt, after.Context, after.UpdatedAt)
	}
}

func TestDeletingOrMovingANoteRemovesIt(t *testing.T) {
	dir := t.TempDir()
	gone := put(t, dir, "gone.md", "# Gone\n")
	moved := put(t, dir, "moved.md", "# Moved\n")
	put(t, dir, "stays.md", "# Stays\n")
	poller, store, _ := newPoller(t, Dir{Path: dir, Label: "ideas"})
	poller.pollOnce()
	if n := len(store.Items()); n != 3 {
		t.Fatalf("%d items, want 3", n)
	}

	os.Remove(gone)
	os.Mkdir(filepath.Join(dir, "done"), 0o700)
	os.Rename(moved, filepath.Join(dir, "done", "moved.md"))
	poller.pollOnce()

	items := store.Items()
	if len(items) != 1 || items[0].ID != "notes:ideas/stays.md" {
		t.Errorf("items = %v, want only stays.md", items)
	}
}

func TestDotfilesSubdirectoriesAndOtherFilesAreIgnored(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, ".hidden.md", "# Hidden\n")
	put(t, dir, "notes.txt", "# Text\n")
	put(t, dir, "real.md", "# Real\n")
	os.Mkdir(filepath.Join(dir, "sub"), 0o700)
	put(t, filepath.Join(dir, "sub"), "nested.md", "# Nested\n")
	os.Mkdir(filepath.Join(dir, "folder.md"), 0o700)
	poller, store, _ := newPoller(t, Dir{Path: dir, Label: "ideas"})

	poller.pollOnce()

	items := store.Items()
	if len(items) != 1 || items[0].Title != "Real" {
		t.Errorf("items = %v, want only real.md", items)
	}
}

func TestFrontmatterDateAndApp(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "a.md", "---\ndate: \"2026-03-04T09:30:00+01:00\"\napp: 'Voice Memos'\ntags: [x]\n---\n# After front\n")
	put(t, dir, "b.md", "---\ndate: 2026-03-04\n---\nbody\n")
	put(t, dir, "c.md", "---\ndate: yesterday-ish\n---\n# C\n")
	put(t, dir, "d.md", "---\ndate: 2026-03-04\n# never closed\n")
	poller, store, _ := newPoller(t, Dir{Path: dir, Label: "ideas"})

	poller.pollOnce()

	a, _ := store.Get("notes:ideas/a.md")
	if a.Title != "After front" || a.Context["captured"] != "2026-03-04T09:30:00+01:00" || a.Context["app"] != "Voice Memos" {
		t.Errorf("a = %q %v", a.Title, a.Context)
	}
	b, _ := store.Get("notes:ideas/b.md")
	if b.Context["captured"] != "2026-03-04T00:00:00Z" {
		t.Errorf("b captured = %q", b.Context["captured"])
	}
	c, _ := store.Get("notes:ideas/c.md")
	if _, has := c.Context["captured"]; has {
		t.Errorf("an unparseable date was published: %v", c.Context)
	}
	d, _ := store.Get("notes:ideas/d.md")
	if _, has := d.Context["captured"]; has || d.Title != "never closed" {
		t.Errorf("an unclosed block was read as frontmatter: %q %v", d.Title, d.Context)
	}
}

func TestMissingDirectoryWarnsAndTheRestStillWork(t *testing.T) {
	good := t.TempDir()
	put(t, good, "a.md", "# A\n")
	missing := filepath.Join(t.TempDir(), "not-there")
	poller, store, logs := newPoller(t,
		Dir{Path: missing, Label: "gone"}, Dir{Path: good, Label: "ideas"})

	poller.pollOnce()
	poller.pollOnce()

	if items := store.Items(); len(items) != 1 || items[0].ID != "notes:ideas/a.md" {
		t.Errorf("items = %v", items)
	}
	status := poller.SourceStatus()
	if !status.Healthy || status.Items != 1 || !strings.Contains(status.Warning, `"gone"`) {
		t.Errorf("status = %+v", status)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("logged %d warnings in two polls, want 1:\n%s", n, logs)
	}

	// The directory appears: its notes show up on the next poll.
	if err := os.Mkdir(missing, 0o700); err != nil {
		t.Fatal(err)
	}
	put(t, missing, "b.md", "# B\n")
	poller.pollOnce()
	if n := len(store.Items()); n != 2 {
		t.Errorf("%d items after the directory appeared, want 2", n)
	}
	if w := poller.SourceStatus().Warning; w != "" {
		t.Errorf("warning %q outlived the problem", w)
	}
}

func TestOversizedNoteIsSkippedAndLoggedOnce(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "big.md", "# Big\n"+strings.Repeat("x", MaxSize))
	put(t, dir, "small.md", "# Small\n")
	poller, store, logs := newPoller(t, Dir{Path: dir, Label: "ideas"})

	poller.pollOnce()
	poller.pollOnce()

	if items := store.Items(); len(items) != 1 || items[0].Title != "Small" {
		t.Errorf("items = %v", items)
	}
	if n := strings.Count(logs.String(), "larger than"); n != 1 {
		t.Errorf("oversize logged %d times, want 1:\n%s", n, logs)
	}
}

func TestSameFileNameInTwoDirectoriesStaysApart(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	put(t, a, "idea.md", "# From A\n")
	put(t, b, "idea.md", "# From B\n")
	poller, store, _ := newPoller(t, Dir{Path: a, Label: "work"}, Dir{Path: b, Label: "home"})

	poller.pollOnce()

	x, _ := store.Get("notes:work/idea.md")
	y, _ := store.Get("notes:home/idea.md")
	if x.Title != "From A" || y.Title != "From B" {
		t.Errorf("work=%q home=%q", x.Title, y.Title)
	}
}

//go:build unix

package notes

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFIFOInTheDirectoryIsSkippedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.md"), 0o600); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	put(t, dir, "real.md", "# Real\n")
	poller, store, _ := newPoller(t, Dir{Path: dir, Label: "ideas"})

	done := make(chan struct{})
	go func() {
		poller.pollOnce()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poll blocked on a FIFO")
	}

	items := store.Items()
	if len(items) != 1 || items[0].Title != "Real" {
		t.Errorf("items = %v, want only real.md", items)
	}
}

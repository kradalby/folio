package history

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/scottjab/folio/internal/vault"
)

func TestRecordingNeverWaitsForTheWorktree(t *testing.T) {
	v, err := vault.Open(t.TempDir())
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	t.Cleanup(func() { v.Close() })

	c := New(Options{Quiet: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(c.Close)
	if err := c.Watch(1, v, Person{Email: "owner@github"}); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	r := c.repoFor(1)

	// A flush is busy with the disk, and will be for a while.
	r.mu.Lock()
	defer r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.record("a.md", change{act: actCreated, by: Person{Email: "alice@github"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("record waited on the worktree lock; the bus would drop a burst of events during a long commit")
	}
}

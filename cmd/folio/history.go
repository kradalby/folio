package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/scottjab/folio/internal/history"
	"github.com/scottjab/folio/internal/identity"
)

// vaultHistory is the glue between the app and the git committer: it knows
// which vaults exist, how to turn a user row into a commit author, and when to
// sweep for repositories worth repacking.
//
// A nil *vaultHistory means history is switched off, and every method is a
// no-op on it, so the call sites in serve and dev read the same either way.
type vaultHistory struct {
	c   *history.Committer
	app *app
	log *slog.Logger
}

// newVaultHistory returns nil when the configuration has git turned off.
func newVaultHistory(a *app, log *slog.Logger) *vaultHistory {
	if !a.cfg.Git.Enabled {
		return nil
	}
	return &vaultHistory{
		c: history.New(history.Options{
			Quiet: a.cfg.Git.Quiet.Duration(),
			Bus:   a.bus,
			Log:   log,
		}),
		app: a,
		log: log,
	}
}

// startAll puts every existing vault under watch, committing whatever changed
// while folio was not running, then starts the repack sweep if there is one.
func (h *vaultHistory) startAll(ctx context.Context) error {
	if h == nil {
		return nil
	}
	rows, err := h.app.vaultRows(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !h.app.dirExists(row.Dir) {
			continue // doctor reports these; there is nothing to commit
		}
		h.start(row.ID, row.Dir, author(row.Login, row.DisplayName))
	}
	if every := h.app.cfg.Git.GCEvery.Duration(); every > 0 {
		go h.gcLoop(ctx, every)
	}
	return nil
}

// startFor watches a vault that has just been created.
func (h *vaultHistory) startFor(u identity.User) {
	if h == nil {
		return
	}
	h.start(u.VaultID, u.VaultDir, author(u.Login, u.DisplayName))
}

func (h *vaultHistory) start(vaultID int64, dir string, owner history.Person) {
	v, err := h.app.vaults.Get(dir)
	if err != nil {
		h.log.Warn("cannot open vault to keep its history", "vault", dir, "err", err)
		return
	}
	// A refusal here (a vault inside someone else's repository, say) is a
	// reason to skip that vault, not to stop the server.
	if err := h.c.Watch(vaultID, v, owner); err != nil {
		h.log.Warn("not keeping history for vault", "vault", dir, "err", err)
	}
}

// gcLoop sweeps the repositories now and then every interval until ctx ends.
// The first sweep is immediate rather than an interval away, so a server that
// is restarted every day still gets one; a sweep with nothing to do is cheap.
func (h *vaultHistory) gcLoop(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		h.c.GC(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// stop commits anything pending and stops listening.
func (h *vaultHistory) stop() {
	if h == nil {
		return
	}
	h.c.Close()
}

// author is the commit identity for a user: their display name, with the
// tailnet login where an email would go.
func author(login, displayName string) history.Person {
	return history.Person{Name: displayName, Email: login}
}

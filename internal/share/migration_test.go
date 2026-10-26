package share_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/store"
)

func TestLegacyGrantsPreserveRowsAndRequireRegrant(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := os.ReadFile("../store/migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		PRAGMA user_version = 1;
		INSERT INTO users(id, tailscale_user_id, login, vault_dir, created_at) VALUES
			(1, 1, 'alice', 'alice', 0), (2, 2, 'bob', 'bob', 0),
			(3, 3, 'duplicate', 'duplicate-a', 0), (4, 4, 'DUPLICATE', 'duplicate-b', 0),
			(6, 6, 'recycled', 'original', 0);
		INSERT INTO vaults(id, user_id, dir, created_at) VALUES (1, 1, 'alice', 0), (2, 2, 'bob', 0);
		INSERT INTO shares(id, vault_id, owner_user_id, path, is_folder, grantee_login, perm, created_at) VALUES
			('lower', 1, 1, 'shared.md', 0, 'bob', 'write', 1),
			('upper', 1, 1, 'shared.md', 1, 'BOB', 'read', 2),
			('ambiguous', 1, 1, 'ambiguous.md', 0, 'duplicate', 'read', 3),
			('missing', 1, 1, 'missing.md', 0, 'missing', 'read', 4),
			('reused-before-upgrade', 1, 1, 'reused.md', 0, 'recycled', 'read', 5);
		UPDATE users SET login = 'original-renamed' WHERE id = 6;
		INSERT INTO users(id, tailscale_user_id, login, vault_dir, created_at)
			VALUES (7, 7, 'recycled', 'replacement', 0);
	`); err != nil {
		t.Fatal(err)
	}
	const snapshot = `SELECT group_concat(id || ':' || vault_id || ':' || owner_user_id || ':' || path || ':' ||
		is_folder || ':' || grantee_login || ':' || perm || ':' || created_at, '|')
		FROM (SELECT * FROM shares ORDER BY id)`
	var before string
	if err := legacy.QueryRow(snapshot).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	after, err := db.One[string](ctx, snapshot)
	if err != nil || after != before {
		t.Fatalf("migration changed existing grants: before=%q after=%q error=%v", before, after, err)
	}
	r := share.NewResolver(db)
	if err := r.Check(ctx, identity.User{ID: 7, Login: "recycled"}, 1, "reused.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("pre-upgrade login reuse reassigned a legacy grant: %v", err)
	}
	alice := identity.User{ID: 1, Login: "alice", VaultID: 1}
	bob := identity.User{ID: 2, Login: "bob", VaultID: 2}
	if err := r.Check(ctx, alice, 1, "shared.md", share.Write); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"shared.md", "shared.md/child.md"} {
		if err := r.Check(ctx, bob, 1, path, share.Read); !errors.Is(err, share.ErrDenied) {
			t.Errorf("legacy grant activated without owner regrant on %s: %v", path, err)
		}
	}
	if grants, err := r.SharedWithMe(ctx, bob); err != nil || len(grants) != 0 {
		t.Errorf("legacy grants visible to grantee: %v, %v", grants, err)
	}
	if grants, err := r.SharesIMade(ctx, alice); err != nil || len(grants) != 5 {
		t.Errorf("owner lost legacy grant administration: %v, %v", grants, err)
	}
	for _, id := range []int64{3, 4} {
		u := identity.User{ID: id, Login: "duplicate"}
		if err := r.Check(ctx, u, 1, "ambiguous.md", share.Read); !errors.Is(err, share.ErrDenied) {
			t.Errorf("legacy ambiguous grant authorized user %d: %v", id, err)
		}
	}
	if _, err := r.Grant(ctx, alice, "new.md", false, "duplicate", share.Read); !errors.Is(err, identity.ErrUnknownUser) {
		t.Errorf("ambiguous new grant = %v, want unknown user", err)
	}
	updated, err := r.Grant(ctx, alice, "shared.md", false, "BoB", share.Read)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != "lower" || updated.CreatedAt != 1 {
		t.Errorf("update changed persisted grant metadata: %+v", updated)
	}
	if err := r.Check(ctx, bob, 1, "shared.md", share.Write); !errors.Is(err, share.ErrDenied) {
		t.Errorf("case-variant legacy row retained write after downgrade: %v", err)
	}
	if err := r.Check(ctx, bob, 1, "shared.md", share.Read); err != nil {
		t.Errorf("explicit owner regrant denied: %v", err)
	}
	if err := r.Check(ctx, bob, 1, "shared.md/child.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("note regrant retained old folder scope: %v", err)
	}
	count, err := db.One[int](ctx, "SELECT count(*) FROM shares WHERE id IN ('lower', 'upper')")
	if err != nil || count != 2 {
		t.Errorf("duplicate legacy grant rows lost: %d, %v", count, err)
	}
	ident := identity.NewResolver(db, func(context.Context, string) (identity.WhoIs, error) {
		return identity.WhoIs{UserID: 5, Login: "missing"}, nil
	}, identity.Options{})
	newcomer, err := ident.Identify(ctx, "100.64.0.5:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "UPDATE users SET login = 'distinct' WHERE id = 4"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r = share.NewResolver(db)
	if err := r.Check(ctx, bob, 1, "shared.md", share.Read); err != nil {
		t.Errorf("reopen lost explicit owner regrant: %v", err)
	}
	if err := r.Check(ctx, identity.User{ID: 3, Login: "duplicate"}, 1, "ambiguous.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("reopen rebound resolved duplicate label: %v", err)
	}
	if err := r.Check(ctx, newcomer, 1, "missing.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("new identity inherited unbound legacy grant: %v", err)
	}
	if grant, err := r.Grant(ctx, alice, "missing.md", false, "missing", share.Read); err != nil || grant.ID != "missing" {
		t.Fatalf("explicit regrant must keep legacy ID: %+v, %v", grant, err)
	}
	if err := r.Check(ctx, newcomer, 1, "missing.md", share.Read); err != nil {
		t.Fatalf("explicit owner regrant denied: %v", err)
	}
	if err := r.Revoke(ctx, alice, "ambiguous"); err != nil {
		t.Fatalf("owner cannot revoke unbound legacy grant: %v", err)
	}
	if err := r.Revoke(ctx, alice, "lower"); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(ctx, bob, 1, "shared.md", share.Read); err != nil {
		t.Errorf("per-ID revoke removed preserved duplicate grant: %v", err)
	}
}

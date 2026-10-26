package share_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/scottjab/folio/internal/identity"
	"github.com/scottjab/folio/internal/share"
	"github.com/scottjab/folio/internal/store"
)

func TestGrantsFollowIdentityAcrossLoginReuse(t *testing.T) {
	e := newEnv(t)
	grant, err := e.res.Grant(e.ctx, e.alice, "shared.md", false, e.bob.Login, share.Write)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "shared.md", share.Write); err != nil {
		t.Fatal(err)
	}
	var who identity.WhoIs
	resolver := identity.NewResolver(e.db, func(context.Context, string) (identity.WhoIs, error) {
		return who, nil
	}, identity.Options{})
	rename := func(id int64, login string) identity.User {
		t.Helper()
		who = identity.WhoIs{UserID: id, Login: login}
		u, err := resolver.Identify(e.ctx, "100.64.0.1:1")
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	bob := rename(2, "bob-renamed@github")
	carol := rename(3, "bob@github")
	if bob.ID != e.bob.ID || bob.VaultID != e.bob.VaultID || bob.VaultDir != e.bob.VaultDir {
		t.Fatalf("rename changed persisted owner/vault: before=%+v after=%+v", e.bob, bob)
	}
	for _, tc := range []struct {
		name string
		user identity.User
		want bool
	}{
		{"renamed grantee", bob, true},
		{"cached grantee", e.bob, true},
		{"reused login", carol, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := share.NewResolver(e.db)
			err := r.Check(e.ctx, tc.user, e.alice.VaultID, "shared.md", share.Write)
			if (err == nil) != tc.want || (err != nil && !errors.Is(err, share.ErrDenied)) {
				t.Errorf("Check = %v, want allowed=%v", err, tc.want)
			}
			ids, err := r.ReadableVaults(e.ctx, tc.user)
			if err != nil || slices.Contains(ids, e.alice.VaultID) != tc.want {
				t.Errorf("ReadableVaults = %v, %v, want grant=%v", ids, err, tc.want)
			}
			grants, err := r.SharedWithMe(e.ctx, tc.user)
			if err != nil || (len(grants) == 1 && grants[0].ID == grant.ID) != tc.want {
				t.Errorf("SharedWithMe = %v, %v, want grant=%v", grants, err, tc.want)
			}
			paths, err := r.FilterReadable(e.ctx, tc.user, e.alice.VaultID, []string{"shared.md", "private.md"})
			if err != nil || slices.Contains(paths, "shared.md") != tc.want || slices.Contains(paths, "private.md") {
				t.Errorf("FilterReadable = %v, %v, want grant=%v", paths, err, tc.want)
			}
			perm, err := r.PermFor(e.ctx, tc.user, e.alice.VaultID, "shared.md")
			wantPerm := share.Perm("")
			if tc.want {
				wantPerm = share.Write
			}
			if err != nil || perm != wantPerm {
				t.Errorf("PermFor = %v, %v, want grant=%v", perm, err, tc.want)
			}
		})
	}
	if _, err := e.res.Grant(e.ctx, e.alice, "carol-only.md", false, carol.Login, share.Read); err != nil {
		t.Fatal(err)
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "carol-only.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("cached Bob inherited Carol's new grant: %v", err)
	}
	path, err := e.db.One[string](e.ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'")
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := share.NewResolver(reopened).Check(e.ctx, bob, e.alice.VaultID, "shared.md", share.Write); err != nil {
		t.Errorf("reopened grant lost intended grantee: %v", err)
	}
	carolGrant, err := e.res.Grant(e.ctx, e.alice, "shared.md", false, carol.Login, share.Read)
	if err != nil || carolGrant.ID == grant.ID {
		t.Fatalf("independent grant to reused login = %+v, %v", carolGrant, err)
	}
	otherOwnerGrant, err := e.res.Grant(e.ctx, e.carol, "shared.md", true, bob.Login, share.Write)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := e.res.Grant(e.ctx, e.alice, "shared.md", false, bob.Login, share.Read)
	if err != nil || updated.ID != grant.ID {
		t.Fatalf("update original identity grant = %+v, %v", updated, err)
	}
	if err := e.res.Check(e.ctx, bob, e.carol.VaultID, "shared.md/child.md", share.Write); err != nil {
		t.Errorf("regrant changed another owner's scope/permission: %v", err)
	}
	otherGrants, err := e.res.SharesIMade(e.ctx, e.carol)
	if err != nil || len(otherGrants) != 1 || otherGrants[0].ID != otherOwnerGrant.ID {
		t.Errorf("regrant changed another owner's grant IDs: %v, %v", otherGrants, err)
	}
	if _, err := e.res.Grant(e.ctx, e.bob, "self.md", false, bob.Login, share.Read); err == nil {
		t.Error("stale owner login allowed a self grant")
	}
	if err := e.res.Revoke(e.ctx, e.alice, grant.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.res.Check(e.ctx, bob, e.alice.VaultID, "shared.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("owner revoke after rename = %v, want ErrDenied", err)
	}
	if err := e.res.Check(e.ctx, carol, e.alice.VaultID, "shared.md", share.Read); err != nil {
		t.Errorf("revoking Bob's grant also revoked Carol's: %v", err)
	}
}

func TestGrantsRejectDuplicateLoginArrival(t *testing.T) {
	e := newEnv(t)
	if _, err := e.res.Grant(e.ctx, e.alice, "shared.md", false, e.bob.Login, share.Read); err != nil {
		t.Fatal(err)
	}
	r := identity.NewResolver(e.db, func(context.Context, string) (identity.WhoIs, error) {
		return identity.WhoIs{UserID: 3, Login: e.bob.Login}, nil
	}, identity.Options{})
	carol, err := r.Identify(e.ctx, "100.64.0.3:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.res.Check(e.ctx, e.bob, e.alice.VaultID, "shared.md", share.Read); err != nil {
		t.Errorf("duplicate label removed original grantee access: %v", err)
	}
	if err := e.res.Check(e.ctx, carol, e.alice.VaultID, "shared.md", share.Read); !errors.Is(err, share.ErrDenied) {
		t.Errorf("newcomer inherited original grantee access: %v", err)
	}
	if _, err := e.res.Grant(e.ctx, e.alice, "new.md", false, e.bob.Login, share.Read); !errors.Is(err, identity.ErrUnknownUser) {
		t.Errorf("ambiguous new grant = %v, want unknown user", err)
	}
}

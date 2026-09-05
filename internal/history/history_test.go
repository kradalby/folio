package history_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/scottjab/folio/internal/events"
	"github.com/scottjab/folio/internal/history"
	"github.com/scottjab/folio/internal/vault"
)

// These run against real repositories on disk with a short quiet period, and
// wait for commits to appear rather than reaching into the committer. The
// debouncer's timing is already covered deterministically in internal/watch.

const (
	quiet   = 25 * time.Millisecond
	vaultID = int64(7)
)

var (
	owner = history.Person{Name: "Vault Owner", Email: "owner@github"}
	alice = history.Person{Name: "Alice", Email: "alice@github"}
	bob   = history.Person{Name: "Bob", Email: "bob@github"}
)

type fixture struct {
	t     *testing.T
	v     *vault.Vault
	bus   *events.Bus
	c     *history.Committer
	clock time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	v, err := vault.Open(t.TempDir())
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	t.Cleanup(func() { v.Close() })

	f := &fixture{t: t, v: v, bus: events.NewBus(), clock: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	f.c = history.New(history.Options{
		Quiet: quiet,
		Bus:   f.bus,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:   func() time.Time { return f.clock },
	})
	t.Cleanup(f.c.Close)
	return f
}

// watch puts the vault under watch, which makes the initial commit.
func (f *fixture) watch() {
	f.t.Helper()
	if err := f.c.Watch(vaultID, f.v, owner); err != nil {
		f.t.Fatalf("Watch: %v", err)
	}
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	if _, err := f.v.Write(path, []byte(content), ""); err != nil {
		f.t.Fatalf("Write %s: %v", path, err)
	}
}

// note emits a NoteChanged as the notes service would.
func (f *fixture) note(kind, path, oldPath string, by history.Person) {
	f.clock = f.clock.Add(time.Second)
	f.bus.Emit(context.Background(), events.NoteChanged{
		ID: "ev", Kind: kind, VaultID: vaultID, Vault: "v", Path: path, OldPath: oldPath,
		ByLogin: by.Email, ByName: by.Name, At: f.clock,
	})
}

func (f *fixture) file(path string, by history.Person) {
	f.clock = f.clock.Add(time.Second)
	f.bus.Emit(context.Background(), events.FileChanged{
		VaultID: vaultID, Vault: "v", Path: path, ByLogin: by.Email, ByName: by.Name, At: f.clock,
	})
}

// log returns the commits reachable from HEAD, newest first. It tolerates a
// repository that is mid-write by returning what it could read.
func (f *fixture) log() []*object.Commit {
	repo, err := git.PlainOpen(f.v.Dir())
	if err != nil {
		return nil
	}
	iter, err := repo.Log(&git.LogOptions{})
	if err != nil {
		return nil
	}
	var out []*object.Commit
	iter.ForEach(func(c *object.Commit) error {
		out = append(out, c)
		return nil
	})
	return out
}

// waitCommits polls until the repository has n commits.
func (f *fixture) waitCommits(n int) []*object.Commit {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := f.log(); len(got) >= n {
			if len(got) > n {
				f.t.Fatalf("expected %d commits, found %d:\n%s", n, len(got), subjects(got))
			}
			return got
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %d commits; have:\n%s", n, subjects(f.log()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settle waits long enough that any pending batch would have been committed,
// for the tests that assert nothing more happened.
func settle() { time.Sleep(10 * quiet) }

func subjects(cs []*object.Commit) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString("  ")
		b.WriteString(strings.SplitN(c.Message, "\n", 2)[0])
		b.WriteByte('\n')
	}
	return b.String()
}

// files lists every path in a commit's tree.
func files(t *testing.T, c *object.Commit) []string {
	t.Helper()
	iter, err := c.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	var out []string
	iter.ForEach(func(f *object.File) error {
		out = append(out, f.Name)
		return nil
	})
	slices.Sort(out)
	return out
}

func subject(c *object.Commit) string { return strings.SplitN(c.Message, "\n", 2)[0] }

func TestWatchStartsARepositoryAndCommitsWhatIsThere(t *testing.T) {
	f := newFixture(t)
	f.write("Daily/2026-09-04.md", "# Today\n")
	f.write("attachments/shot.png", "not really a png")
	f.watch()

	cs := f.waitCommits(1)
	head := cs[0]
	if got := subject(head); got != "Start keeping history" {
		t.Errorf("subject = %q", got)
	}
	want := []string{".gitignore", "Daily/2026-09-04.md", "attachments/shot.png"}
	if got := files(t, head); !slices.Equal(got, want) {
		t.Errorf("tree = %v, want %v", got, want)
	}
	if head.Author.Name != owner.Name || head.Author.Email != owner.Email {
		t.Errorf("author = %s <%s>, want the owner", head.Author.Name, head.Author.Email)
	}
	if head.Committer.Name != "folio" {
		t.Errorf("committer = %q, want folio", head.Committer.Name)
	}

	repo, err := git.PlainOpen(f.v.Dir())
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	ref, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if ref.Name() != plumbing.Main {
		t.Errorf("branch = %s, want main", ref.Name())
	}
}

func TestNothingToRecordMeansNoCommit(t *testing.T) {
	f := newFixture(t)
	f.write("a.md", "a\n")
	f.watch()
	f.waitCommits(1)

	// Close and reopen the committer on the same vault: nothing changed, so
	// nothing should be recorded.
	f.c.Close()
	f.c = history.New(history.Options{Quiet: quiet, Bus: f.bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	f.watch()
	if got := f.log(); len(got) != 1 {
		t.Errorf("rewatching an unchanged vault made a commit:\n%s", subjects(got))
	}
}

func TestChangesMadeWhileAwayAreCommittedOnWatch(t *testing.T) {
	f := newFixture(t)
	f.write("a.md", "a\n")
	f.watch()
	f.waitCommits(1)
	f.c.Close()

	// Obsidian had an afternoon with the vault.
	f.write("a.md", "a, revised\n")
	f.write("b.md", "b\n")

	f.c = history.New(history.Options{Quiet: quiet, Bus: f.bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	f.watch()
	cs := f.waitCommits(2)
	if got := subject(cs[0]); got != "Record changes made while folio was not running" {
		t.Errorf("subject = %q", got)
	}
	if got := files(t, cs[0]); !slices.Contains(got, "b.md") {
		t.Errorf("tree %v is missing b.md", got)
	}
}

func TestACommitFollowsTheQuietPeriod(t *testing.T) {
	f := newFixture(t)
	f.watch()
	f.waitCommits(1)

	f.write("Projects/folio.md", "# folio\n")
	f.note(events.NoteCreated, "Projects/folio.md", "", alice)

	cs := f.waitCommits(2)
	head := cs[0]
	if got := subject(head); got != "Create Projects/folio.md" {
		t.Errorf("subject = %q", got)
	}
	if head.Author.Name != "Alice" || head.Author.Email != "alice@github" {
		t.Errorf("author = %s <%s>, want Alice <alice@github>", head.Author.Name, head.Author.Email)
	}
	if !head.Author.When.Equal(f.clock) {
		t.Errorf("author date = %v, want the change's time %v", head.Author.When, f.clock)
	}
	if got := files(t, head); !slices.Contains(got, "Projects/folio.md") {
		t.Errorf("tree %v is missing the note", got)
	}
}

func TestABurstIsOneCommit(t *testing.T) {
	f := newFixture(t)
	f.watch()
	f.waitCommits(1)

	for _, p := range []string{"a.md", "b.md", "c.md"} {
		f.write(p, p+"\n")
		f.note(events.NoteCreated, p, "", alice)
	}
	// Editing one of them again inside the same quiet period is still one
	// commit, and the note is still "created" in it.
	f.write("a.md", "a, again\n")
	f.note(events.NoteUpdated, "a.md", "", alice)

	cs := f.waitCommits(2)
	settle()
	if got := f.log(); len(got) != 2 {
		t.Fatalf("a burst produced %d commits, want 2:\n%s", len(got), subjects(got))
	}
	msg := cs[0].Message
	if got := subject(cs[0]); got != "Create 3 notes" {
		t.Errorf("subject = %q", got)
	}
	for _, want := range []string{"created a.md\n", "created b.md\n", "created c.md\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message should list %q; got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "(alice") {
		t.Errorf("a single author should not be named on every line:\n%s", msg)
	}
}

func TestMixedAuthorsAreNamedInTheBody(t *testing.T) {
	f := newFixture(t)
	f.watch()
	f.waitCommits(1)

	f.write("a.md", "a\n")
	f.note(events.NoteCreated, "a.md", "", alice)
	f.write("b.md", "b\n")
	f.note(events.NoteCreated, "b.md", "", bob)

	cs := f.waitCommits(2)
	head := cs[0]
	if head.Author.Email != bob.Email {
		t.Errorf("author = %s, want the most recent contributor, bob", head.Author.Email)
	}
	for _, want := range []string{"created a.md (alice@github)", "created b.md (bob@github)"} {
		if !strings.Contains(head.Message, want) {
			t.Errorf("message should contain %q; got:\n%s", want, head.Message)
		}
	}
}

func TestMoveAndDelete(t *testing.T) {
	f := newFixture(t)
	f.write("old.md", "content\n")
	f.watch()
	f.waitCommits(1)

	if err := f.v.Move("old.md", "new.md"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	f.note(events.NoteMoved, "new.md", "old.md", alice)
	cs := f.waitCommits(2)
	if got := subject(cs[0]); got != "Move old.md to new.md" {
		t.Errorf("subject = %q", got)
	}
	got := files(t, cs[0])
	if slices.Contains(got, "old.md") || !slices.Contains(got, "new.md") {
		t.Errorf("tree after move = %v", got)
	}

	if err := f.v.Delete("new.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	f.note(events.NoteDeleted, "new.md", "", alice)
	cs = f.waitCommits(3)
	if got := subject(cs[0]); got != "Delete new.md" {
		t.Errorf("subject = %q", got)
	}
	got = files(t, cs[0])
	if slices.Contains(got, "new.md") {
		t.Errorf("deleted note still in tree: %v", got)
	}
	// The delete moved the file into the trash, which must never be committed.
	for _, p := range got {
		if strings.HasPrefix(p, ".folio/") {
			t.Errorf("sidecar path committed: %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(f.v.Dir(), ".folio", "trash")); err != nil {
		t.Fatalf("the trash should still be on disk: %v", err)
	}
}

func TestCreatedThenDeletedIsNotMentioned(t *testing.T) {
	f := newFixture(t)
	f.watch()
	f.waitCommits(1)

	f.write("keep.md", "keep\n")
	f.note(events.NoteCreated, "keep.md", "", alice)
	f.write("scratch.md", "oops\n")
	f.note(events.NoteCreated, "scratch.md", "", alice)
	if err := f.v.Delete("scratch.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	f.note(events.NoteDeleted, "scratch.md", "", alice)

	cs := f.waitCommits(2)
	if got := subject(cs[0]); got != "Create keep.md" {
		t.Errorf("subject = %q; a note that came and went inside one batch should not appear", got)
	}
}

func TestAttachmentsAreCommitted(t *testing.T) {
	f := newFixture(t)
	f.watch()
	f.waitCommits(1)

	f.write("attachments/diagram.png", "png bytes")
	f.file("attachments/diagram.png", alice)

	cs := f.waitCommits(2)
	// The event cannot say whether an attachment is new; the index can.
	if got := subject(cs[0]); got != "Create attachments/diagram.png" {
		t.Errorf("subject = %q", got)
	}
	if got := files(t, cs[0]); !slices.Contains(got, "attachments/diagram.png") {
		t.Errorf("tree %v is missing the attachment", got)
	}
}

func TestNotesAndFilesTogetherAreFiles(t *testing.T) {
	f := newFixture(t)
	f.write("a.md", "a\n")
	f.watch()
	f.waitCommits(1)

	f.write("a.md", "a, revised\n")
	f.note(events.NoteUpdated, "a.md", "", alice)
	f.write("a.png", "png")
	f.file("a.png", alice)

	cs := f.waitCommits(2)
	if got := subject(cs[0]); got != "Change 2 files" {
		t.Errorf("subject = %q", got)
	}
}

func TestExternalChangesAreAuthoredByTheOwnerAndDescribedByGit(t *testing.T) {
	f := newFixture(t)
	f.write("old.md", "old\n")
	f.watch()
	f.waitCommits(1)

	// The watcher reports every external change as an update, because it
	// cannot tell. Git can, and the message should say what git saw.
	f.write("new.md", "written by Obsidian\n")
	f.note(events.NoteUpdated, "new.md", "", history.Person{})
	f.write("old.md", "rewritten by Obsidian\n")
	f.note(events.NoteUpdated, "old.md", "", history.Person{})

	cs := f.waitCommits(2)
	if cs[0].Author.Email != owner.Email {
		t.Errorf("author = %s, want the owner", cs[0].Author.Email)
	}
	if got := subject(cs[0]); got != "Change 2 notes" {
		t.Errorf("subject = %q", got)
	}
	for _, want := range []string{"created new.md\n", "updated old.md\n"} {
		if !strings.Contains(cs[0].Message, want) {
			t.Errorf("message should say %q; got:\n%s", want, cs[0].Message)
		}
	}
}

func TestTheVaultsGitignoreIsObeyed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("Private/\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	v, err := vault.Open(dir)
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	t.Cleanup(func() { v.Close() })

	f := newFixture(t)
	f.v = v
	f.watch()
	f.waitCommits(1)

	f.write("Private/secret.md", "shh\n")
	f.note(events.NoteCreated, "Private/secret.md", "", alice)
	f.write("Public/hello.md", "hi\n")
	f.note(events.NoteCreated, "Public/hello.md", "", alice)

	cs := f.waitCommits(2)
	if got := subject(cs[0]); got != "Create Public/hello.md" {
		t.Errorf("subject = %q; the ignored note should not be mentioned", got)
	}
	for _, p := range files(t, cs[0]) {
		if strings.HasPrefix(p, "Private/") {
			t.Errorf("ignored path was committed: %s", p)
		}
	}
	// And the user's .gitignore did not say .folio/, so the built-in exclusion
	// has to be what keeps the sidecar out.
	if err := f.v.Delete("Public/hello.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	f.note(events.NoteDeleted, "Public/hello.md", "", alice)
	cs = f.waitCommits(3)
	for _, p := range files(t, cs[0]) {
		if strings.HasPrefix(p, ".folio/") {
			t.Errorf("sidecar path committed despite a .gitignore that does not mention it: %s", p)
		}
	}
}

func TestAnUnchangedSaveIsNotACommit(t *testing.T) {
	f := newFixture(t)
	f.write("a.md", "same\n")
	f.watch()
	f.waitCommits(1)

	f.write("a.md", "same\n")
	f.note(events.NoteUpdated, "a.md", "", alice)
	settle()
	if got := f.log(); len(got) != 1 {
		t.Errorf("a save that changed nothing produced a commit:\n%s", subjects(got))
	}
}

func TestCloseCommitsWhatIsPending(t *testing.T) {
	v, err := vault.Open(t.TempDir())
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	defer v.Close()
	bus := events.NewBus()
	c := history.New(history.Options{Quiet: time.Hour, Bus: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := c.Watch(vaultID, v, owner); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	if _, err := v.Write("a.md", []byte("late edit\n"), ""); err != nil {
		t.Fatalf("Write: %v", err)
	}
	bus.Emit(context.Background(), events.NoteChanged{
		Kind: events.NoteCreated, VaultID: vaultID, Path: "a.md", ByLogin: alice.Email, At: time.Now(),
	})
	c.Close()

	repo, err := git.PlainOpen(v.Dir())
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
	if got := subject(commit); got != "Create a.md" {
		t.Errorf("subject = %q; Close should have flushed the pending change", got)
	}
}

func TestRefusesAVaultInsideAnotherRepository(t *testing.T) {
	parent := t.TempDir()
	if _, err := git.PlainInit(parent, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	v, err := vault.Open(filepath.Join(parent, "vault"))
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	defer v.Close()

	c := history.New(history.Options{Quiet: quiet, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer c.Close()
	err = c.Watch(vaultID, v, owner)
	if err == nil || !strings.Contains(err.Error(), "inside the git repository") {
		t.Fatalf("Watch inside another repo: err = %v, want a refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(v.Dir(), ".git")); statErr == nil {
		t.Error("a nested repository was created anyway")
	}
}

func TestAnExistingRepositoryIsUsedAsItStands(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	// Someone has been keeping this vault in git by hand, on master.
	wt, _ := repo.Worktree()
	os.WriteFile(filepath.Join(dir, "mine.md"), []byte("mine\n"), 0o644)
	wt.Add("mine.md")
	if _, err := wt.Commit("theirs", &git.CommitOptions{Author: &object.Signature{Name: "Me", Email: "me@x", When: time.Now()}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	v, err := vault.Open(dir)
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	t.Cleanup(func() { v.Close() })
	f := newFixture(t)
	f.v = v
	f.watch()

	// vault.Open added a .gitignore, which is the one change to record. The
	// history before folio is untouched underneath it.
	cs := f.waitCommits(2)
	if got := subject(cs[1]); got != "theirs" {
		t.Errorf("the existing history was not preserved; oldest commit is %q", got)
	}
	if got := subject(cs[0]); got != "Record changes made while folio was not running" {
		t.Errorf("subject = %q", got)
	}
	ref, _ := git.PlainOpen(dir)
	head, _ := ref.Head()
	if head.Name() != plumbing.Master {
		t.Errorf("folio switched branches to %s", head.Name())
	}
}

// objectCounts counts loose objects and packfiles by looking at the .git
// directory directly, so the test does not trust the code under test to count.
func objectCounts(t *testing.T, dir string) (loose, packs int) {
	t.Helper()
	objects := filepath.Join(dir, ".git", "objects")
	entries, err := os.ReadDir(objects)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		switch {
		case e.IsDir() && len(e.Name()) == 2:
			inner, err := os.ReadDir(filepath.Join(objects, e.Name()))
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			loose += len(inner)
		case e.IsDir() && e.Name() == "pack":
			inner, err := os.ReadDir(filepath.Join(objects, "pack"))
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			for _, p := range inner {
				if strings.HasSuffix(p.Name(), ".pack") {
					packs++
				}
			}
		}
	}
	return loose, packs
}

func TestGCRepacksOnceObjectsPileUp(t *testing.T) {
	// One loose object is already too many.
	t.Cleanup(history.SetGCThresholds(1, 50))

	f := newFixture(t)
	f.write("a.md", "a\n")
	f.watch()
	f.waitCommits(1)
	if loose, _ := objectCounts(t, f.v.Dir()); loose == 0 {
		t.Fatal("a commit should have left loose objects behind")
	}

	f.c.GC(context.Background())
	if loose, packs := objectCounts(t, f.v.Dir()); loose != 0 || packs != 1 {
		t.Errorf("after GC: %d loose objects and %d packs, want 0 and 1", loose, packs)
	}
	cs := f.log()
	if len(cs) != 1 || !slices.Contains(files(t, cs[0]), "a.md") {
		t.Fatalf("history after GC:\n%s", subjects(cs))
	}

	// Life goes on: the repacked repository still takes commits, and a second
	// repack folds the new pack and the old one into one.
	f.write("b.md", "b\n")
	f.note(events.NoteCreated, "b.md", "", alice)
	cs = f.waitCommits(2)
	f.c.GC(context.Background())
	if loose, packs := objectCounts(t, f.v.Dir()); loose != 0 || packs != 1 {
		t.Errorf("after the second GC: %d loose objects and %d packs, want 0 and 1", loose, packs)
	}
	cs = f.log()
	if len(cs) != 2 {
		t.Fatalf("history after the second GC:\n%s", subjects(cs))
	}
	got := files(t, cs[0])
	for _, want := range []string{"a.md", "b.md"} {
		if !slices.Contains(got, want) {
			t.Errorf("tree %v is missing %s after repacking", got, want)
		}
	}

	// And a third commit after that, to be sure the reopened handle is sound.
	f.write("c.md", "c\n")
	f.note(events.NoteCreated, "c.md", "", alice)
	f.waitCommits(3)
}

func TestGCLeavesATidyRepositoryAlone(t *testing.T) {
	f := newFixture(t)
	f.watch()
	f.waitCommits(1)

	before, _ := objectCounts(t, f.v.Dir())
	f.c.GC(context.Background())
	after, packs := objectCounts(t, f.v.Dir())
	if after != before || packs != 0 {
		t.Errorf("GC touched a repository under the thresholds: %d -> %d loose, %d packs", before, after, packs)
	}
}

// noteAs emits a NoteChanged made by an agent acting as a person.
func (f *fixture) noteAs(kind, path string, by history.Person, agent string) {
	f.clock = f.clock.Add(time.Second)
	f.bus.Emit(context.Background(), events.NoteChanged{
		ID: "ev", Kind: kind, VaultID: vaultID, Vault: "v", Path: path,
		ByLogin: by.Email, ByName: by.Name, ByAgent: agent, At: f.clock,
	})
}

func TestAnAgentActingAsYouIsNamedOnItsCommits(t *testing.T) {
	f := newFixture(t)
	f.watch()
	f.waitCommits(1)

	f.write("a.md", "written by the agent\n")
	f.noteAs(events.NoteCreated, "a.md", alice, "tag:notes-agent")
	cs := f.waitCommits(2)
	head := cs[0]
	if head.Author.Name != "Alice via tag:notes-agent" || head.Author.Email != "alice@github" {
		t.Errorf("author = %s <%s>, want Alice via tag:notes-agent <alice@github>", head.Author.Name, head.Author.Email)
	}

	// Alice and her agent in one batch are two contributors, so the body says
	// which change was whose.
	f.write("b.md", "by alice herself\n")
	f.note(events.NoteCreated, "b.md", "", alice)
	f.write("c.md", "by the agent\n")
	f.noteAs(events.NoteCreated, "c.md", alice, "tag:notes-agent")
	cs = f.waitCommits(3)
	for _, want := range []string{"created b.md (alice@github)\n", "created c.md (alice@github via tag:notes-agent)\n"} {
		if !strings.Contains(cs[0].Message, want) {
			t.Errorf("message should contain %q; got:\n%s", want, cs[0].Message)
		}
	}
}

func TestAnUnreadableIndexIsRebuilt(t *testing.T) {
	f := newFixture(t)
	f.write("a.md", "a\n")
	f.watch()
	f.waitCommits(1)
	f.c.Close()

	// A crash or a kill mid-write leaves a truncated index. Meanwhile the
	// vault kept changing.
	if err := os.WriteFile(filepath.Join(f.v.Dir(), ".git", "index"), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f.write("b.md", "b\n")

	f.c = history.New(history.Options{Quiet: quiet, Bus: f.bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := f.c.Watch(vaultID, f.v, owner); err != nil {
		t.Fatalf("Watch should heal an unreadable index, not fail on it: %v", err)
	}
	cs := f.waitCommits(2)
	got := files(t, cs[0])
	for _, want := range []string{"a.md", "b.md"} {
		if !slices.Contains(got, want) {
			t.Errorf("tree %v is missing %s after the index was rebuilt", got, want)
		}
	}

	// And history goes on from there.
	f.write("c.md", "c\n")
	f.note(events.NoteCreated, "c.md", "", alice)
	f.waitCommits(3)
}

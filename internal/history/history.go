// Package history keeps each vault in a git repository.
//
// Every change to a vault is recorded as a commit once the vault has been quiet
// for a while: a note saved in the browser, an edit made by an agent, a file
// Obsidian wrote, an image dropped in. The commit is authored by whoever made
// the change, as the tailnet identified them, so `git log` in a vault answers
// "who changed this, and when" without anyone having configured a git identity.
//
// folio commits and does nothing else. It never pushes, pulls, merges, or
// switches branches. The repository is the user's, folio is one more thing
// writing to it, and it is careful to stay out of the way of a person doing the
// same by hand: a vault that is already a repository is used as it stands, and
// the vault's own .gitignore is obeyed.
//
// The work is done with go-git rather than by running git, so folio stays one
// static binary and the service needs nothing installed beside it. That choice
// shapes the code in one place: go-git's status pass hashes every file in the
// worktree, which for a vault full of attachments is the wrong thing to do
// every few minutes. So the committer stages exactly the paths the events
// named and reads the whole vault only once, at startup, to catch what changed
// while folio was not running.
package history

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
	gitindex "github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"

	"github.com/scottjab/folio/internal/events"
	"github.com/scottjab/folio/internal/vault"
	"github.com/scottjab/folio/internal/vaultpath"
	"github.com/scottjab/folio/internal/watch"
)

// DefaultQuiet is how long a vault is left alone before its changes are
// committed when the caller does not say.
const DefaultQuiet = 5 * time.Minute

// maxDelayFactor bounds how long sustained writing can postpone a commit. Every
// autosave resets the quiet timer, so an hour of steady typing would otherwise
// produce one commit at the end and no way back to the middle. With the default
// quiet period this forces a commit every half hour at most.
const maxDelayFactor = 6

// ErrClosed means the Committer has been shut down.
var ErrClosed = errors.New("history committer is closed")

// committer is the identity on every commit's committer line. The author is
// the person who made the change; the committer is the thing that wrote it down.
var committer = Person{Name: "folio", Email: "folio@localhost"}

// excludes are never committed, whatever the vault's own .gitignore says. The
// sidecar holds half-written files and the trash, and neither is content.
var excludes = []gitignore.Pattern{gitignore.ParsePattern(".folio/", nil)}

// Person is who a commit is attributed to. Email carries the tailnet login,
// which is not always an email address; git does not mind. Via names the
// agent that acted for the person, when one did, so a commit an agent made as
// you reads "You via tag:notes-agent <you@github>": yours to search for by
// login, and honest about who typed.
type Person struct {
	Name  string
	Email string
	Via   string
}

// IsZero reports whether nobody in particular is named.
func (p Person) IsZero() bool { return p == Person{} }

// String is how a person is named in a commit body: the login, and the agent
// that acted for them if one did.
func (p Person) String() string {
	if p.Via != "" {
		return p.Email + " via " + p.Via
	}
	return p.Email
}

func (p Person) signature(at time.Time) *object.Signature {
	name := p.Name
	if name == "" {
		name = p.Email
	}
	if p.Via != "" {
		name += " via " + p.Via
	}
	return &object.Signature{Name: name, Email: p.Email, When: at}
}

// Options configures a Committer.
type Options struct {
	// Quiet is how long a vault must go without a change before what has
	// changed is committed. Zero means DefaultQuiet.
	Quiet time.Duration
	// Bus is where the committer hears about changes. With no bus it still
	// records the state of each vault when asked to watch it, and nothing after.
	Bus *events.Bus
	Log *slog.Logger
	// Now is the clock stamped on commits. Tests replace it.
	Now func() time.Time
}

// Committer records changes to vaults as git commits.
type Committer struct {
	quiet time.Duration
	bus   *events.Bus
	log   *slog.Logger
	now   func() time.Time

	unsubscribe []func()

	mu     sync.Mutex
	repos  map[int64]*repo
	closed bool
}

// New returns a Committer that is listening but watching no vaults yet.
func New(opts Options) *Committer {
	c := &Committer{
		quiet: opts.Quiet,
		bus:   opts.Bus,
		log:   opts.Log,
		now:   opts.Now,
		repos: map[int64]*repo{},
	}
	if c.quiet <= 0 {
		c.quiet = DefaultQuiet
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.bus != nil {
		c.unsubscribe = append(c.unsubscribe,
			c.bus.On(func(_ context.Context, e events.NoteChanged) { c.onNote(e) }),
			c.bus.On(func(_ context.Context, e events.FileChanged) { c.onFile(e) }),
		)
	}
	return c
}

// Watch starts recording v, whose vault id is what its events carry and whose
// owner is the author of any change nobody else can be blamed for.
//
// It opens the repository at the vault's root, creating one if there is none,
// and before returning commits anything that changed since the last commit, so
// an afternoon in Obsidian while folio was down still ends up in the history.
// That first pass reads every file in the vault once, which on a vault holding
// gigabytes of attachments takes a few seconds.
//
// A vault that sits inside some other repository is refused rather than given
// a nested one of its own.
func (c *Committer) Watch(vaultID int64, v *vault.Vault, owner Person) error {
	c.mu.Lock()
	closed, watching := c.closed, c.repos[vaultID] != nil
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if watching {
		return nil
	}

	// The first pass reads the whole vault and can take a while, so it runs
	// without the committer's lock: other vaults' changes keep being recorded
	// meanwhile rather than backing up the bus. This vault is registered only
	// once the pass is done, so an external edit to it during those seconds is
	// not lost, just late: the next event for that path stages the whole file.
	r, err := c.open(v.Dir(), owner)
	if err != nil {
		return err
	}
	if err := r.commitAll(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if _, ok := c.repos[vaultID]; ok {
		return nil // a concurrent Watch got there first; theirs is as good as ours
	}
	c.repos[vaultID] = r
	return nil
}

// Close stops listening and commits whatever is still pending. An edit made a
// moment before shutdown is still an edit.
func (c *Committer) Close() {
	for _, u := range c.unsubscribe {
		u()
	}
	if c.bus != nil {
		// Events already queued for us are still on their way.
		c.bus.Wait()
	}

	c.mu.Lock()
	c.closed = true
	repos := make([]*repo, 0, len(c.repos))
	for _, r := range c.repos {
		repos = append(repos, r)
	}
	clear(c.repos)
	c.mu.Unlock()

	for _, r := range repos {
		r.deb.Close()
	}
}

// open returns the repository at dir, initializing one if needed.
func (c *Committer) open(dir string, owner Person) (*repo, error) {
	g, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{DetectDotGit: true})
	switch {
	case errors.Is(err, git.ErrRepositoryNotExists):
		g, err = git.PlainInitWithOptions(dir, &git.PlainInitOptions{
			InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
		})
		if err != nil {
			return nil, fmt.Errorf("start a git repository in %s: %w", dir, err)
		}
		c.log.Info("started a git repository for the vault", "vault", dir)
	case err != nil:
		return nil, fmt.Errorf("open the git repository in %s: %w", dir, err)
	}

	wt, err := g.Worktree()
	if err != nil {
		return nil, fmt.Errorf("open the worktree of %s: %w", dir, err)
	}
	// DetectDotGit walks upwards, so a vault living inside someone's larger
	// repository opens that one. Committing a vault into it, or nesting a second
	// repository, would both be surprises; do neither.
	if root := wt.Filesystem.Root(); !samePath(root, dir) {
		return nil, fmt.Errorf("vault %s is inside the git repository at %s; folio will not commit into it or nest another", dir, root)
	}
	wt.Excludes = excludes
	if err := c.healIndex(g, dir); err != nil {
		return nil, err
	}

	r := &repo{
		dir:     dir,
		owner:   owner,
		git:     g,
		wt:      wt,
		log:     c.log.With("vault", filepath.Base(dir)),
		now:     c.now,
		pending: map[string]change{},
	}
	r.deb = watch.NewDebouncer(c.quiet, maxDelayFactor*c.quiet, r.flush)
	return r, nil
}

// healIndex throws away an index go-git cannot read, so the first pass can
// rebuild it.
//
// go-git rewrites .git/index in place rather than through a lockfile and a
// rename, so a crash or a kill mid-write leaves a truncated file; and an index
// written by a newer git can carry extensions go-git does not understand.
// Either way the index is only a record of what is staged. The first pass
// re-stages the worktree, and if nothing changed the tree matches HEAD and no
// commit is made. What is lost is anything someone had staged by hand and not
// yet committed, which is why this is logged rather than silent.
func (c *Committer) healIndex(g *git.Repository, dir string) error {
	_, err := g.Storer.Index()
	if err == nil {
		return nil
	}
	fss, ok := g.Storer.(*filesystem.Storage)
	if !ok {
		return fmt.Errorf("index of %s is unreadable: %w", dir, err)
	}
	if rmErr := fss.Filesystem().Remove("index"); rmErr != nil {
		return fmt.Errorf("index of %s is unreadable (%v) and could not be removed: %w", dir, err, rmErr)
	}
	c.log.Warn("the git index was unreadable and has been discarded; the vault will be staged afresh",
		"vault", dir, "err", err)
	return nil
}

// samePath reports whether two directory paths name the same directory, seeing
// through symlinks when it can.
func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil {
		return filepath.Clean(ra) == filepath.Clean(rb)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func (c *Committer) repoFor(vaultID int64) *repo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	return c.repos[vaultID]
}

func (c *Committer) onNote(e events.NoteChanged) {
	r := c.repoFor(e.VaultID)
	if r == nil {
		return
	}
	ch := change{by: personOf(e.ByLogin, e.ByName, e.ByAgent), at: e.At}
	switch e.Kind {
	case events.NoteCreated:
		ch.act = actCreated
		r.record(e.Path, ch)
	case events.NoteDeleted:
		ch.act = actDeleted
		r.record(e.Path, ch)
	case events.NoteMoved:
		gone := ch
		gone.act = actVacated
		r.record(e.OldPath, gone)
		ch.act, ch.from = actMoved, e.OldPath
		r.record(e.Path, ch)
	default:
		ch.act = actUpdated
		r.record(e.Path, ch)
	}
}

func (c *Committer) onFile(e events.FileChanged) {
	r := c.repoFor(e.VaultID)
	if r == nil {
		return
	}
	r.record(e.Path, change{act: actChanged, by: personOf(e.ByLogin, e.ByName, e.ByAgent), at: e.At})
}

// personOf builds the author for an event. No login means the change was made
// outside folio, and nobody is named.
func personOf(login, name, agent string) Person {
	if login == "" {
		return Person{}
	}
	return Person{Name: name, Email: login, Via: agent}
}

// repo is one vault's repository and the changes waiting to be committed to it.
type repo struct {
	dir   string
	owner Person
	git   *git.Repository
	wt    *git.Worktree
	deb   *watch.Debouncer
	log   *slog.Logger
	now   func() time.Time

	// mu serializes everything that touches the worktree or the index. go-git
	// is not safe for concurrent use, and neither is a git index.
	mu sync.Mutex

	// pendMu guards pending on its own, so recording a change never waits for
	// a flush that is busy with the disk. The bus gives each subscriber a
	// bounded queue and drops what does not fit, so a handler that blocked
	// behind a long commit would turn a burst of edits into lost history.
	pendMu  sync.Mutex
	pending map[string]change
}

// action is what happened to a path, as far as the commit message is concerned.
type action int

const (
	// actChanged is what an event says when it does not know more: a file that
	// is not a note, or nothing at all. Staging replaces it with what git saw.
	actChanged action = iota
	actCreated
	actUpdated
	actDeleted
	// actMoved marks the destination of a move; change.from is the source.
	actMoved
	// actVacated marks the source of a move. It is staged so the old index
	// entry goes away, and never listed: the destination's line covers it.
	actVacated
)

// verb is the action's imperative for a subject line and its past tense for
// the body.
func (a action) verb() (imperative, past string) {
	switch a {
	case actCreated:
		return "Create", "created"
	case actUpdated:
		return "Update", "updated"
	case actDeleted:
		return "Delete", "deleted"
	case actMoved:
		return "Move", "moved"
	default:
		return "Change", "changed"
	}
}

// change is one pending edit to one path.
type change struct {
	act  action
	from string
	// by is who made the change, or zero for a change made outside folio.
	by Person
	at time.Time
}

// record notes that path changed and arms the timer.
func (r *repo) record(path string, ch change) {
	r.pendMu.Lock()
	keep := true
	if old, ok := r.pending[path]; ok {
		ch, keep = merge(old, ch)
	}
	if keep {
		r.pending[path] = ch
	} else {
		delete(r.pending, path)
	}
	r.pendMu.Unlock()
	// Staging a path git has never seen is a no-op, so the debouncer hears
	// about the path whether or not there is anything left to say about it.
	r.deb.Add(path)
}

// merge folds a new change for a path into the one already pending, so a note
// created and then edited three times inside one quiet period is "created". A
// note created and then deleted is nothing at all, and keep is false.
func merge(old, next change) (merged change, keep bool) {
	if next.by.IsZero() {
		next.by = old.by
	}
	switch {
	case old.act == actCreated && next.act == actDeleted:
		return change{}, false
	case old.act == actCreated && next.act == actUpdated:
		next.act = actCreated
	case old.act == actMoved && next.act == actUpdated:
		next.act, next.from = actMoved, old.from
	}
	return next, true
}

// flush is the debouncer's callback: stage what changed and commit it.
func (r *repo) flush(paths []string) {
	r.pendMu.Lock()
	batch := make(map[string]change, len(paths))
	for _, p := range paths {
		if ch, ok := r.pending[p]; ok {
			batch[p] = ch
			delete(r.pending, p)
		}
	}
	r.pendMu.Unlock()

	r.mu.Lock()
	err := r.commitBatch(paths, batch)
	r.mu.Unlock()
	if err == nil {
		return
	}

	r.log.Warn("could not commit; will try again with the next batch", "err", err)
	// Put the batch back so a disk that was full, or a permission that was
	// wrong, costs a delay rather than a hole in the history. A change that
	// arrived in the meantime is newer and wins.
	r.pendMu.Lock()
	for p, ch := range batch {
		if _, ok := r.pending[p]; !ok {
			r.pending[p] = ch
		}
	}
	r.pendMu.Unlock()
	for p := range batch {
		r.deb.Add(p)
	}
}

func (r *repo) commitBatch(paths []string, batch map[string]change) error {
	staged, err := r.stage(paths)
	if err != nil {
		return err
	}
	if len(staged) == 0 {
		// A save that changed nothing, or an edit undone within the quiet
		// period. There is nothing to say.
		return nil
	}

	msg, author, at := describe(batch, staged, r.owner, r.now())
	hash, err := r.commit(msg, author, at)
	if errors.Is(err, git.ErrEmptyCommit) {
		return nil
	}
	if err != nil {
		return err
	}
	r.log.Info("committed", "commit", short(hash), "files", len(staged), "subject", firstLine(msg))
	return nil
}

// stage brings the index in line with the worktree for exactly these paths and
// returns the ones whose index entry actually changed as a result, with what
// the change was. Paths the vault's .gitignore covers are left alone: someone
// who ignores a folder meant it.
func (r *repo) stage(paths []string) ([]stagedFile, error) {
	patterns, err := gitignore.ReadPatterns(r.wt.Filesystem, nil)
	if err != nil {
		r.log.Debug("could not read every .gitignore; using what was readable", "err", err)
	}
	ignored := gitignore.NewMatcher(append(patterns, excludes...))

	before, err := r.entries(paths)
	if err != nil {
		return nil, err
	}
	var considered []string
	for _, p := range paths {
		if ignored.Match(strings.Split(p, "/"), false) {
			continue
		}
		if err := r.stageOne(p, before[p] != nil); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		considered = append(considered, p)
	}

	after, err := r.entries(considered)
	if err != nil {
		return nil, err
	}
	var staged []stagedFile
	for _, p := range considered {
		if act, changed := indexDelta(before[p], after[p]); changed {
			staged = append(staged, stagedFile{path: p, act: act})
		}
	}
	return staged, nil
}

// stagedFile is one path whose index entry moved, and what the move says
// happened to it. That verdict comes from git rather than from the event, which
// matters for changes made outside folio: the watcher can only ever say
// "something happened to this path", and git knows whether the file is new.
type stagedFile struct {
	path string
	act  action
}

// stageOne is `git add -A -- path` for a single file: add or update it if it is
// there, drop it from the index if it is not.
func (r *repo) stageOne(p string, tracked bool) error {
	_, err := os.Lstat(filepath.Join(r.dir, filepath.FromSlash(p)))
	if errors.Is(err, fs.ErrNotExist) {
		if !tracked {
			return nil // came and went without git ever seeing it
		}
		idx, err := r.git.Storer.Index()
		if err != nil {
			return err
		}
		if _, err := idx.Remove(p); err != nil {
			return err
		}
		return r.git.Storer.SetIndex(idx)
	}
	if err != nil {
		return err
	}
	// SkipStatus is the point of staging path by path. Without it go-git works
	// out the status of the whole worktree first, hashing every file in the
	// vault to add one note.
	return r.wt.AddWithOptions(&git.AddOptions{Path: p, SkipStatus: true})
}

// entries reads the index once and looks up each path in it. A path that is
// not tracked maps to nil.
func (r *repo) entries(paths []string) (map[string]*gitindex.Entry, error) {
	idx, err := r.git.Storer.Index()
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	out := make(map[string]*gitindex.Entry, len(paths))
	for _, p := range paths {
		e, err := idx.Entry(p)
		if errors.Is(err, gitindex.ErrEntryNotFound) {
			out[p] = nil
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("index entry %s: %w", p, err)
		}
		out[p] = e
	}
	return out, nil
}

// indexDelta reports whether staging moved an index entry, and if so how: it
// appeared, it disappeared, or it now points at different content.
func indexDelta(before, after *gitindex.Entry) (action, bool) {
	switch {
	case before == nil && after == nil:
		return actChanged, false
	case before == nil:
		return actCreated, true
	case after == nil:
		return actDeleted, true
	case before.Hash != after.Hash || before.Mode != after.Mode:
		return actUpdated, true
	}
	return actChanged, false
}

// commitAll stages everything and commits it, for the moment a vault comes
// under watch. It is the one place the whole worktree is read.
func (r *repo) commitAll() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return fmt.Errorf("stage %s: %w", r.dir, err)
	}
	msg := "Record changes made while folio was not running"
	if _, err := r.git.Head(); errors.Is(err, plumbing.ErrReferenceNotFound) {
		msg = "Start keeping history"
	}
	now := r.now()
	hash, err := r.commit(msg, r.owner, now)
	if errors.Is(err, git.ErrEmptyCommit) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("commit %s: %w", r.dir, err)
	}
	r.log.Info("committed", "commit", short(hash), "subject", msg)
	return nil
}

func (r *repo) commit(msg string, author Person, at time.Time) (plumbing.Hash, error) {
	return r.wt.Commit(msg, &git.CommitOptions{
		Author:    author.signature(at),
		Committer: committer.signature(r.now()),
	})
}

// describe writes the commit message for a batch and decides who it is by.
//
// The author is whoever made the most recent change in the batch, falling back
// to the vault's owner when every change came from outside folio. When more
// than one person contributed, each line of the body says who.
//
// What happened to each path is taken from the event when folio made the
// change, since only the event knows a move from a delete and a create. For
// everything else, a change from outside or an attachment, it is taken from what
// staging did to the index.
func describe(batch map[string]change, staged []stagedFile, owner Person, now time.Time) (msg string, author Person, at time.Time) {
	type line struct {
		path string
		ch   change
	}
	var lines []line
	authors := map[Person]bool{}
	var lastAuthored time.Time
	for _, s := range staged {
		p := s.path
		ch, ok := batch[p]
		if !ok || ch.by.IsZero() || ch.act == actChanged {
			ch.act = s.act
		}
		if !ch.by.IsZero() {
			authors[ch.by] = true
			// The most recent change by a person names the author, even when
			// something external happened after it: a person elsewhere in the
			// batch is a better author than nobody.
			if !ch.at.Before(lastAuthored) {
				lastAuthored, author = ch.at, ch.by
			}
		}
		if ch.at.After(at) {
			at = ch.at
		}
		if ch.act == actVacated {
			continue
		}
		lines = append(lines, line{p, ch})
	}
	slices.SortFunc(lines, func(a, b line) int { return strings.Compare(a.path, b.path) })
	if author.IsZero() {
		author = owner
	}
	if at.IsZero() {
		at = now
	}

	var b strings.Builder
	switch len(lines) {
	case 0:
		// Only the sources of moves, which cannot happen: a move always lists
		// its destination. Say something true anyway.
		b.WriteString("Update the vault")
	case 1:
		l := lines[0]
		imp, _ := l.ch.act.verb()
		if l.ch.act == actMoved {
			fmt.Fprintf(&b, "Move %s to %s", l.ch.from, l.path)
		} else {
			fmt.Fprintf(&b, "%s %s", imp, l.path)
		}
	default:
		imp, _ := lines[0].ch.act.verb()
		noun := "notes"
		for _, l := range lines {
			if v, _ := l.ch.act.verb(); v != imp {
				imp = "Change"
			}
			if !vaultpath.IsMarkdown(l.path) {
				noun = "files"
			}
		}
		fmt.Fprintf(&b, "%s %d %s\n\n", imp, len(lines), noun)
		for _, l := range lines {
			_, past := l.ch.act.verb()
			if l.ch.act == actMoved {
				fmt.Fprintf(&b, "%s %s to %s", past, l.ch.from, l.path)
			} else {
				fmt.Fprintf(&b, "%s %s", past, l.path)
			}
			if len(authors) > 1 {
				if l.ch.by.IsZero() {
					b.WriteString(" (outside folio)")
				} else {
					fmt.Fprintf(&b, " (%s)", l.ch.by)
				}
			}
			b.WriteByte('\n')
		}
	}
	return b.String(), author, at
}

// Repack thresholds, borrowed from `git gc --auto`: gc.auto is 6700 loose
// objects and gc.autoPackLimit is 50 packs. Below both a repository is left
// alone, because a repack rewrites every object it has, and a vault full of
// attachments should not pay that for a few dozen small files.
var (
	gcLooseObjects = 6700
	gcPacks        = 50
)

// GC repacks every watched repository whose loose objects or packfiles have
// piled up past the thresholds git itself uses, and leaves the rest alone. It
// is meant to run on a schedule: a sweep that finds nothing to do costs one
// directory listing per vault.
//
// A repack is the one thing here that rewrites history's storage rather than
// adding to it, so it is worth saying what it does: every object reachable from
// a ref goes into one new pack, the loose copies and the old packs are deleted,
// and anything unreachable is dropped with them, as `git gc --prune=now` would.
// folio never creates unreachable objects; a `git reset` done by hand does.
func (c *Committer) GC(ctx context.Context) {
	c.mu.Lock()
	repos := slices.Collect(maps.Values(c.repos))
	c.mu.Unlock()

	for _, r := range repos {
		if ctx.Err() != nil {
			return
		}
		if err := r.gc(); err != nil {
			r.log.Warn("could not repack the repository", "err", err)
		}
	}
}

func (r *repo) gc() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	loose, packs, err := r.objectCounts()
	if err != nil {
		return err
	}
	if loose <= gcLooseObjects && packs <= gcPacks {
		return nil
	}

	start := time.Now()
	if err := r.git.RepackObjects(&git.RepackConfig{}); err != nil {
		return fmt.Errorf("repack: %w", err)
	}
	// go-git remembers the index of every pack it has read, and deleting a pack
	// does not make it forget, so the next object lookup could go looking in a
	// file that is gone. A fresh handle has nothing to forget.
	if err := r.reopen(); err != nil {
		return err
	}
	r.log.Info("repacked", "loose_objects", loose, "packs", packs,
		"took", time.Since(start).Round(time.Millisecond))
	return nil
}

// objectCounts is how many loose objects and how many packs the repository has.
func (r *repo) objectCounts() (loose, packs int, err error) {
	los, ok := r.git.Storer.(storer.LooseObjectStorer)
	pos, ok2 := r.git.Storer.(storer.PackedObjectStorer)
	if !ok || !ok2 {
		return 0, 0, errors.New("repository storage does not support repacking")
	}
	if err := los.ForEachObjectHash(func(plumbing.Hash) error { loose++; return nil }); err != nil {
		return 0, 0, fmt.Errorf("count loose objects: %w", err)
	}
	hs, err := pos.ObjectPacks()
	if err != nil {
		return 0, 0, fmt.Errorf("list packs: %w", err)
	}
	return loose, len(hs), nil
}

// reopen replaces the repository and worktree handles with fresh ones. The
// caller holds r.mu.
func (r *repo) reopen() error {
	g, err := git.PlainOpen(r.dir)
	if err != nil {
		return fmt.Errorf("reopen %s: %w", r.dir, err)
	}
	wt, err := g.Worktree()
	if err != nil {
		return fmt.Errorf("reopen worktree of %s: %w", r.dir, err)
	}
	wt.Excludes = excludes
	r.git, r.wt = g, wt
	return nil
}

func short(h plumbing.Hash) string { return h.String()[:8] }

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

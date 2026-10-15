package croft

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// This file is the snapshot-native delta: a tree-level three-way merge of two
// change sets against their common base layer, replacing the git-era
// ApplyDelta's `git merge-tree` over a carried binary patch. Nothing is
// carried in the state — the merge is computed from two daemon-side diffs
// (base → theirs, base → ours) at call time.

// mergeChanges performs the tree-level merge: theirs (base → the incoming
// state) against ours (base → the current workspace). Both change lists must
// be sorted by Path, which the tar parser guarantees. The content maps carry
// each side's post-images from its own diff blob, so the identical-edit check
// compares bytes without touching either workspace; a missing map entry means
// "no content" (the parser omits empty-file bodies, directories, and special
// files), and bytes.Equal treats nil and empty alike — two empty files are
// identical.
//
// Per-path rules:
//
//   - only theirs changed → take theirs: the entry joins apply.
//   - only ours changed → keep ours: nothing to do, no entry.
//   - both changed → delete+delete keeps; non-deletes with identical bytes
//     AND identical mode keep; every other combination (modify/delete,
//     delete/modify, divergent edits, mode disagreement, type change) is a
//     conflict.
//
// One rule reaches beyond single paths: a rigid entry on one side — a
// delete, or any non-directory change — conflicts with any non-delete
// change strictly underneath it on the other side (theirs removes dir/
// while ours wrote dir/f; theirs replaces dir/ with a file while ours
// added dir/f). The per-path walk cannot see it — the paths differ — but
// the intents contradict, and silently applying both would drop the kept
// child, resurrect the deleted subtree, or write a file through a path
// the other side turned into a directory. Directories are the one
// flexible type: children coexist with them. Deletes underneath a rigid
// entry are compatible (both sides removing), and deleted directories
// carry per-child whiteouts in their own side's change set, so this rule
// fires only when the child appeared after the base — exactly the
// divergent-intent case.
//
// conflicts carries theirs-side entries: the incoming paths the merge could
// not reconcile, which is what the ErrDeltaConflict error names. It is sorted
// by path and deduplicated; apply keeps theirs' order minus any path demoted
// to a conflict. The function is pure — callers compute both change sets and
// hold all content before any mutation, so a non-empty conflicts list means
// the workspace was never touched.
func mergeChanges(theirs, ours []Change, theirsContent, oursContent map[string][]byte) (apply []Change, conflicts []Change) {
	// Per-path walk over the two sorted lists.
	conflicted := make(map[string]bool)
	addConflict := func(c Change) {
		if !conflicted[c.Path] {
			conflicted[c.Path] = true
			conflicts = append(conflicts, c)
		}
	}

	ti, oi := 0, 0
	for ti < len(theirs) && oi < len(ours) {
		t, o := theirs[ti], ours[oi]
		switch {
		case t.Path < o.Path:
			apply = append(apply, t)
			ti++
		case o.Path < t.Path:
			oi++ // keep ours
		default:
			ti++
			oi++
			if !identicalChanges(t, o, theirsContent, oursContent) {
				addConflict(t)
			} // else keep: both sides made the same change
		}
	}
	apply = append(apply, theirs[ti:]...)

	// Rigid-vs-underneath collisions, in both directions. The conflict entry
	// is always theirs-side: theirs' rigid entry when it collides with ours,
	// theirs' underneath entry when ours is the rigid side.
	for _, t := range theirs {
		if !isRigid(t) {
			continue
		}
		forEachUnder(ours, t.Path, func(o Change) bool {
			if o.Kind != ChangeDelete {
				addConflict(t)
				return false
			}
			return true
		})
	}
	for _, o := range ours {
		if !isRigid(o) {
			continue
		}
		forEachUnder(theirs, o.Path, func(t Change) bool {
			if t.Kind != ChangeDelete {
				addConflict(t)
			}
			return true
		})
	}

	if len(conflicts) > 0 {
		// Conflicted paths never join the apply set: refusal is all-or-
		// nothing at the caller, but the algebra stays honest in isolation.
		kept := apply[:0]
		for _, a := range apply {
			if !conflicted[a.Path] {
				kept = append(kept, a)
			}
		}
		apply = kept
		sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	}
	return apply, conflicts
}

// identicalChanges reports whether both sides made the same change to the
// same path, so the merge can keep it instead of conflicting. Two deletes
// always match — the path is gone from both trees, and delta change sets
// carry no classified metadata on deletes to disagree about. Non-deletes
// match on identical mode (the full fs.FileMode, so type changes and
// special bits count) and identical bytes.
func identicalChanges(t, o Change, theirsContent, oursContent map[string][]byte) bool {
	tDel, oDel := t.Kind == ChangeDelete, o.Kind == ChangeDelete
	if tDel || oDel {
		return tDel && oDel
	}
	if t.Mode != o.Mode || t.IsDir != o.IsDir {
		return false
	}
	return bytes.Equal(theirsContent[t.Path], oursContent[o.Path])
}

// isRigid reports whether an entry contradicts any change underneath its
// path on the other side: deletes and every non-directory type (regular
// file, symlink, special file) do; directories are the one type children
// coexist with.
func isRigid(c Change) bool {
	return c.Kind == ChangeDelete || !c.IsDir
}

// forEachUnder calls fn for every entry whose path lies strictly under dir
// (prefix dir+"/"), in sorted order, stopping early when fn returns false.
// The list must be sorted by Path: the range is found by binary search and
// bounded above by dir+"0", since '/' (0x2F) sorts below every character
// that could follow a completed path segment ('0' is 0x30).
func forEachUnder(list []Change, dir string, fn func(Change) bool) {
	prefix := dir + "/"
	hi := dir + "0"
	for i := sort.Search(len(list), func(i int) bool { return list[i].Path >= prefix }); i < len(list) && list[i].Path < hi; i++ {
		if !fn(list[i]) {
			return
		}
	}
}

// deltaOpKind classifies a planned delta mutation.
type deltaOpKind int

const (
	// opRemove deletes a path (rm -rf: files, symlinks, and directories
	// alike, since a deleted directory's remaining contents go with it).
	opRemove deltaOpKind = iota
	// opMkdir creates a directory and applies its mode.
	opMkdir
	// opWrite writes a regular file's content and mode (WriteFile, which
	// creates parent directories as needed).
	opWrite
	// opSymlink replaces a path with a symlink to target.
	opSymlink
)

// deltaOp is one planned mutation of the ApplyDelta application phase.
// path is workspace-relative; content is the post-image from theirs' diff
// blob (nil for an empty file); mode carries the full fs.FileMode from the
// tar, special bits included.
type deltaOp struct {
	kind    deltaOpKind
	path    string
	content []byte
	mode    fs.FileMode
	target  string
}

// planDelta turns the merged apply set into the concrete mutation sequence,
// validating that every entry is applicable BEFORE anything is written — the
// refusal-is-atomic contract extends to entries the workspace cannot express.
//
// Ordering is load-bearing: removes first (a path theirs deleted and then
// recreated as another type — file → directory — must be cleared before the
// new type is created), then directories (path-sorted, so parents precede
// children), then regular files, then symlinks last (a symlink's target path
// may be created by an earlier op, and ln must not be shadowed by a write to
// the same path). apply arrives sorted by path, and the partition preserves
// that order within each group.
//
// Every write and symlink is preceded by rm -rf of its path: the workspace
// holds the base's entry there (ours never touched it — the merge rules
// guarantee that), and the base's type is unknowable here — the parser
// reports every blob entry as ChangeModify (a diff blob cannot distinguish
// add from modify), and delta runs no classification reader task. The write
// must replace whatever is there: truncate-through on a symlink would
// corrupt its target, on a directory it fails, and on a FIFO open(2) blocks.
// rm -rf is uniformly correct and a cheap no-op when the path is absent (the
// add case). Directories use a guarded script instead: a dir landing on a
// NON-directory (the base's file or symlink-to-directory replaced by a dir)
// must clear it first, but an existing real directory must survive — ours
// may hold kept children in it, which the rigid-entry rule permits. The
// script also clears special bits symbolically before the numeric chmod;
// see deltaMkdirScript.
//
// No size guard: the delta carries full file contents transiently, the same
// documented blob-cost risk the diff machinery carries (the roadmap rules
// MaxDeltaBytes a knob only once a host hits the ceiling).
func planDelta(apply []Change, theirsContent map[string][]byte) ([]deltaOp, error) {
	var removes, mkdirs, writes, symlinks []deltaOp
	for _, c := range apply {
		switch {
		case c.Kind == ChangeDelete:
			removes = append(removes, deltaOp{kind: opRemove, path: c.Path})

		case c.Mode&specialFileMode != 0:
			// A FIFO, socket, or device node in theirs: the workspace has no
			// contract tool to recreate it (mkfifo/mknod are outside the
			// image contract), and writing its "content" would silently
			// create a regular file instead. Refuse before any mutation.
			return nil, fmt.Errorf("cannot apply special file %s: the image contract has no primitive to recreate it", c.Path)

		case c.Mode&fs.ModeSymlink != 0:
			target := string(theirsContent[c.Path])
			if target == "" {
				// The parser captures every symlink's target from the tar's
				// Linkname; a missing one is a corrupt blob, not an empty
				// target (symlink(2) rejects those).
				return nil, fmt.Errorf("cannot apply symlink %s: no target in the delta blob", c.Path)
			}
			symlinks = append(symlinks, deltaOp{kind: opSymlink, path: c.Path, target: target})

		case c.IsDir:
			mkdirs = append(mkdirs, deltaOp{kind: opMkdir, path: c.Path, mode: c.Mode})

		default:
			// Regular file: content from the blob; a missing entry is an
			// empty file (the parser omits empty bodies).
			writes = append(writes, deltaOp{
				kind:    opWrite,
				path:    c.Path,
				content: theirsContent[c.Path],
				mode:    c.Mode,
			})
		}
	}

	ops := make([]deltaOp, 0, len(apply))
	ops = append(ops, removes...)
	ops = append(ops, mkdirs...)
	ops = append(ops, writes...)
	ops = append(ops, symlinks...)
	return ops, nil
}

// ErrDeltaConflict is returned by ApplyDelta when the incoming state's
// changes conflict with the workspace's own changes since their common base:
// both sides touched the same path incompatibly (divergent edits, mode
// disagreement, modify vs delete, type change), or one side's rigid entry
// (delete, file, symlink) contradicts a change underneath it on the other.
// The refusal is atomic — the workspace is untouched — and the error names
// the conflicting paths.
var ErrDeltaConflict = errors.New("croft: delta conflicts with the workspace's own changes")

// The delta application scripts. rm -rf covers files, symlinks, and
// directories alike; the symlink script rm-then-ln's because ln -s refuses
// to replace an existing path. The mkdir script is the subtle one:
//
//   - The guard removes the path unless it is a REAL directory. `[ ! -d ]`
//     alone follows symlinks: a symlink-to-directory would survive, mkdir -p
//     would no-op through it, and the chmod would rewrite the target's mode —
//     possibly outside the workspace. `[ -L ]` closes that; real directories
//     survive because ours may hold kept children inside them.
//   - `chmod -s,-t` precedes the numeric mode because some chmod
//     implementations (GNU-derived semantics, observed on uutils) PRESERVE a
//     directory's setuid/setgid when the numeric mode does not spell the
//     special bits, so a theirs-side 02755→0755 change would silently stay
//     2755. The symbolic clear is portable (GNU, BusyBox, uutils all carry
//     -s/-t); padding the numeric to five digits is not — POSIX numeric
//     modes are at most four octal digits.
//
// The image contract: sh, rm, mkdir, chmod, ln — plus whatever WriteFile
// already needs.
const (
	deltaRemoveScript  = `rm -rf -- "$1"`
	deltaMkdirScript   = "set -e\nif [ ! -d \"$1\" ] || [ -L \"$1\" ]; then rm -rf -- \"$1\"; fi\nmkdir -p -- \"$1\"\nchmod -s,-t \"$1\"\nchmod \"$2\" \"$1\""
	deltaSymlinkScript = `rm -rf -- "$2" && ln -s -- "$1" "$2"`
)

// ApplyDelta merges ws's changes — everything since ws.BaseKey — onto the
// current workspace at the tree level: a three-way merge of two daemon-side
// diffs (base→ws, base→active) computed fully before anything is written.
//
// Mutating on success; the workspace is untouched when it refuses. Errors:
// ErrDeltaConflict (conflicting paths named), ErrStateUnavailable (the base
// or the state's layer has been released). The merge is tree-level, not
// line-level: two divergent edits to the same file conflict even when they
// touch different lines.
//
// A state with an empty BaseKey was captured from a sandbox that never
// Applied — it has no merge base and cannot be merged.
//
// Like WriteFile, ApplyDelta takes the shared operation lock: it mutates
// through the running task and never stops it, so exclusive lifecycle ops
// (Capture/Apply/Destroy) serialize it away. Consistency under concurrent
// Exec equals Diff's: the merge reflects the workspace as of some moment
// during the call. The application phase itself is not transactional — the
// refusal is the atomic part — matching the git-era version's clean-merge
// path.
func (s *ContainerdSandbox) ApplyDelta(ctx context.Context, ws WorkspaceState) error {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	return s.applyDeltaImpl(ctx, ws)
}

// applyDeltaImpl is ApplyDelta's body; opMu is already held (shared).
func (s *ContainerdSandbox) applyDeltaImpl(ctx context.Context, ws WorkspaceState) error {
	if ws.SnapshotKey == "" {
		return fmt.Errorf("containerd sandbox: workspace state has no snapshot key")
	}
	if ws.BaseKey == "" {
		return fmt.Errorf("containerd sandbox: state %s has no base key: it was captured "+
			"from a sandbox that never Applied a state, so there is no merge base to delta against",
			ws.SnapshotKey)
	}
	if err := s.requireBackend(ctx); err != nil {
		return err
	}
	if s.diff == nil {
		return fmt.Errorf("containerd sandbox: diff service not configured")
	}

	s.mu.Lock()
	id, active := s.id, s.activeKey
	s.mu.Unlock()
	if id == "" || active == "" {
		return fmt.Errorf("containerd sandbox: ApplyDelta before Create")
	}

	// Both legs of the three-way merge, each a daemon-side diff computed,
	// read, parsed (with post-images), and unpinned before the next step —
	// the house blob-lease hygiene, on both calls.
	theirs, theirsContent, err := s.computeChangesBetween(ctx, ws.BaseKey, ws.SnapshotKey, true)
	if err != nil {
		return fmt.Errorf("delta theirs (base to state): %w", err)
	}
	ours, oursContent, err := s.computeChangesBetween(ctx, ws.BaseKey, active, true)
	if err != nil {
		return fmt.Errorf("delta ours (base to workspace): %w", err)
	}

	// The merge is pure and complete before the first mutation: a non-empty
	// conflict list means the workspace was never touched.
	apply, conflicts := mergeChanges(theirs, ours, theirsContent, oursContent)
	if len(conflicts) > 0 {
		paths := make([]string, len(conflicts))
		for i, c := range conflicts {
			paths[i] = c.Path
		}
		return fmt.Errorf("%w: %s", ErrDeltaConflict, strings.Join(paths, ", "))
	}
	if len(apply) == 0 {
		return nil // nothing changed on their side, or both sides moved identically
	}

	// Plan (and validate applicability) before the first write.
	ops, err := planDelta(apply, theirsContent)
	if err != nil {
		return fmt.Errorf("containerd sandbox: plan delta: %w", err)
	}

	// Application: removes, dirs, writes, symlinks — planDelta's order. The
	// impls, not the public methods: opMu is already held, and re-RLocking
	// would queue behind any waiting lifecycle op.
	for _, op := range ops {
		resolved := s.resolve(op.path)
		switch op.kind {
		case opRemove:
			if _, err := s.shell(ctx, deltaRemoveScript, resolved); err != nil {
				return fmt.Errorf("containerd sandbox: delta remove %s: %w", op.path, err)
			}
		case opMkdir:
			mode := fmt.Sprintf("%04o", effectivePerm(op.mode))
			// The script's guard replaces a non-directory (or a symlink to
			// one) the base left at the path, without ever removing a real
			// directory ours may hold kept children in; its symbolic
			// pre-clear makes the numeric mode absolute across chmod
			// implementations. See deltaMkdirScript.
			if _, err := s.shell(ctx, deltaMkdirScript, resolved, mode); err != nil {
				return fmt.Errorf("containerd sandbox: delta mkdir %s: %w", op.path, err)
			}
		case opWrite:
			// rm -rf before every write: the base's entry at this path may
			// be any type (the blob cannot distinguish add from modify, and
			// delta runs no classification task), and truncate-through a
			// symlink corrupts its target, truncate on a directory fails,
			// open(2) on a FIFO blocks. A no-op when the path is absent.
			// See planDelta.
			if _, err := s.shell(ctx, deltaRemoveScript, resolved); err != nil {
				return fmt.Errorf("containerd sandbox: delta replace %s: %w", op.path, err)
			}
			if err := s.writeFileImpl(ctx, op.path, op.content, op.mode); err != nil {
				return fmt.Errorf("containerd sandbox: delta write: %w", err)
			}
		case opSymlink:
			// rm-then-ln: the path may hold the base's regular file (a
			// type replacement), and ln -s refuses to replace it.
			if _, err := s.shell(ctx, deltaSymlinkScript, op.target, resolved); err != nil {
				return fmt.Errorf("containerd sandbox: delta symlink %s: %w", op.path, err)
			}
		}
	}
	return nil
}

package croft

// WorkspaceState is a captured workspace: the committed snapshot Capture
// returns and Apply consumes. It is a plain record — three keys naming real
// snapshotter state — so it can be stored, logged, and handed back by value.
// A host that wants a content identity for records can hash the struct; croft
// itself has none.
type WorkspaceState struct {
	// SnapshotKey is the committed snapshot this state resumes from.
	SnapshotKey string
	// ParentKey is the snapshot this one was prepared from, recording the
	// layer lineage.
	ParentKey string
	// BaseKey is the snapshot key of the last state the capturing sandbox
	// had Applied — the layer its working tree diverged from. It is the
	// merge base ApplyDelta diffs against: the state's delta spans every
	// layer since BaseKey, not just the last capture. Empty means the
	// sandbox never Applied anything (a fresh lineage), so the state has
	// no merge base and cannot be merged; ApplyDelta rejects it with a
	// clear error. BaseKey and ParentKey are equal on the first capture
	// after an Apply but diverge on repeated captures — the next capture's
	// parent is the previous capture, while the base stays the last applied
	// state. Both are load-bearing.
	BaseKey string
	// Image is the image the workspace was provisioned from.
	Image string
	// ContentDigest is the content-addressed digest of this state's workspace
	// tree, relative to the image rootfs — the daemon differ's blob digest.
	// Two states with identical content (and the same image) carry the same
	// ContentDigest even when their snapshot keys differ, which is what lets a
	// host compare content across a moved timeline. Empty when the capture
	// could not compute it (a sandbox without a diff service, or a differ
	// failure); Capture still succeeds.
	ContentDigest string
}

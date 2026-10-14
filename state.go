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
	// Image is the image the workspace was provisioned from.
	Image string
}

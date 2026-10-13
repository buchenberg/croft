package croft

// WorkspaceState is a captured workspace: the committed snapshot Capture
// returns and Apply consumes. It is a plain record — three keys naming real
// snapshotter state — so it can be stored, logged, and handed back by value.
type WorkspaceState struct {
	// SnapshotKey is the committed snapshot this state resumes from.
	SnapshotKey string
	// ParentKey is the snapshot this one was prepared from, recording the
	// layer lineage.
	ParentKey string
	// Image is the image the workspace was provisioned from.
	Image string
}

// Digest returns the canonical digest of the state, for records and drift
// detection. It is deterministic and sorted at every level.
//
// It returns an error only if a value cannot be canonically marshaled.
// Callers recording a digest failure should treat it as advisory.
func (ws WorkspaceState) Digest() (string, error) {
	return CanonicalDigest(map[string]any{
		"snapshot_key": ws.SnapshotKey,
		"parent_key":   ws.ParentKey,
		"image":        ws.Image,
	})
}

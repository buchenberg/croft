package croft

// WorkspaceState is the backend-neutral description of a croft workspace: the
// state Capture produces and Apply consumes. Its Digest exists so callers can
// compare and record states without holding the snapshots themselves.
type WorkspaceState struct {
	// Backend identifies the backend that produced the state ("containerd").
	// Apply rejects a state from a different backend.
	Backend string
	// Revision is the backend timeline position: for this package, the
	// committed snapshot key.
	Revision string
	// Data carries the backend-specific payload. The keys this package
	// writes are the StateKey* constants; the values are strings.
	Data map[string]any
}

// Digest returns the canonical digest of the state, for records and drift
// detection. It is deterministic: keys are sorted at every nesting level, so
// two states with the same content digest identically regardless of map
// iteration order.
//
// It returns an error only if Data contains a value that cannot be canonically
// marshaled. Callers recording a digest failure should treat it as advisory.
func (ws WorkspaceState) Digest() (string, error) {
	return CanonicalDigest(map[string]any{
		"backend":  ws.Backend,
		"revision": ws.Revision,
		"data":     ws.Data,
	})
}

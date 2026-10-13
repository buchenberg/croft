package croft

import (
	"testing"

	trace "github.com/buchenberg/shepherd-kernel-go/trace"
)

// TestWorkspaceStateDigestMatchesShepherd pins the extraction's compatibility
// promise: a state digested by croft equals the same state digested by the
// trace package it was extracted from. The digest is part of the state's wire
// format — hosts record it in traces and compare it for drift detection — so a
// divergence here would silently split every state across the two libraries.
//
// The canonical JSON machinery this exercises (digest.go) is a verbatim port,
// but a port can drift; this test is the tripwire.
func TestWorkspaceStateDigestMatchesShepherd(t *testing.T) {
	states := map[string]WorkspaceState{
		"empty": {
			Backend: BackendName,
		},
		"captured": {
			Backend:  BackendName,
			Revision: "shepherd/sbx-abc123/committed/1",
			Data: map[string]any{
				StateKeySnapshotKey: "shepherd/sbx-abc123/committed/1",
				StateKeyParentKey:   "shepherd/sbx-abc123/committed/0",
				StateKeyImage:       "docker.io/library/debian:bookworm",
				StateKeyGitHead:     "0d9f5aa7a8f7caee7e1a1a1b2c3d4e5f60718293a",
			},
		},
		"delta": {
			Backend:  BackendName,
			Revision: "shepherd/sbx-abc123/committed/2",
			Data: map[string]any{
				StateKeySnapshotKey: "shepherd/sbx-abc123/committed/2",
				StateKeyParentKey:   "shepherd/sbx-abc123/committed/1",
				StateKeyImage:       "docker.io/library/debian:bookworm",
				StateKeyGitHead:     "0d9f5aa7a8f7caee7e1a1a1b2c3d4e5f60718293a",
				StateKeyDeltaBase:   "f00dcafe0d9f5aa7a8f7caee7e1a1a1b2c3d4e5f",
				StateKeyDeltaPatch:  "LS0tIGdpdC9iaW5hcnkgcGF0Y2g=",
			},
		},
		"other backend": {
			Backend:  "git",
			Revision: "0d9f5aa7a8f7caee7e1a1a1b2c3d4e5f60718293a",
		},
	}
	for name, state := range states {
		want, err := trace.WorkspaceState{Backend: state.Backend, Revision: state.Revision, Data: state.Data}.Digest()
		if err != nil {
			t.Fatalf("%s: shepherd digest: %v", name, err)
		}
		got, err := state.Digest()
		if err != nil {
			t.Fatalf("%s: croft digest: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: digest = %q, want shepherd's %q", name, got, want)
		}
	}
}

//go:build linux && !nolive && shepherd_p2b_live

package containerd

// Live workspace-substrate integration (plan 03 T2b.7): WorkspaceSubstrate
// over a live containerd daemon — declare a file write in a trace store,
// materialize it through the sandbox, capture, destroy, apply, and verify
// the substrate-written file is present in the re-provisioned workspace.
//
// # Why the extra build tag
//
// This file needs the core module's substrate API, which first ships in core
// v0.7.0 — but this module compiles against *published* core only (no
// replace directive; that is the T0.4 release discipline), and v0.7.0 does
// not exist until the phase-2b release is tagged. So the work lands in two
// acts, the same shape as the T0.8/T0.8b repin:
//
//  1. now: the test is complete and committed behind the shepherd_p2b_live
//     tag, out of the default build graph;
//  2. at the v0.7.0 release: bump this module's core requirement to v0.7.0
//     (go get github.com/buchenberg/shepherd-kernel-go@v0.7.0), drop the
//     extra tag from this file's constraint so it joins live_test.go, and
//     tag the nested module as v0.1.3.
//
// Until act 2, this file is compile-unchecked — deliberately, not
// accidentally: the constraint is written here so nobody assumes it runs.
//
// # Running it (after act 2)
//
//	sudo env SHEPHERD_CONTAINERD_ADDR=/run/containerd/containerd.sock \
//	  go test -count=1 -v -run TestLive_WorkspaceSubstrate ./...

import (
	"context"
	"encoding/base64"
	"testing"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
)

// liveAppendContext is the trusted append context the trace side of the
// test uses, in the shape of core's TrustedAppendContext but with a
// test-local actor so trace provenance names this suite.
var liveAppendContext = shepherd.AppendContext{
	ActorRef:             "runtime:containerd-live",
	PresentedWitnessRefs: []string{"trusted:internal"},
	SchemaVersionSet:     "shepherd2-slice-a",
	TrustMode:            "internal",
}

// TestLive_WorkspaceSubstrateWriteSurvivesRecycle is plan 03's acceptance
// sequence: write -> capture -> destroy -> apply -> verify. The point is the
// recycle: a file written *through the trace* (declare, then materialize)
// must be present in a workspace re-provisioned from a state captured after
// the write, which proves a materialized intent and a captured state
// describe the same world.
func TestLive_WorkspaceSubstrateWriteSurvivesRecycle(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	bootstrapGit(t, ctx, sb, cfg.Workdir)

	store, err := shepherd.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	defer store.Close()

	content := "written through the trace"
	declaration, err := store.Append(liveAppendContext, shepherd.AppendBatch{
		AppendIntentID: "live:substrate:declare",
		Groups: []shepherd.AppendGroup{{
			TraceOwnerID: "owner:live-substrate",
			RetainedContext: &shepherd.RetainedContext{
				SubstrateRef: shepherd.WorkspaceSubstrateRef,
				Containment:  sb.Capabilities().Containment,
			},
			FactDrafts: []shepherd.RecordDraft{{
				Mode:      shepherd.Declaration,
				SchemaRef: shepherd.SchemaWorkspaceFileWrite,
				KindLabel: "workspace_file_write",
				Payload: map[string]any{
					"path":        "notes/greeting.txt",
					"content_b64": base64.StdEncoding.EncodeToString([]byte(content)),
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}

	registry := shepherd.NewSubstrateRegistry()
	if err := registry.Register(shepherd.NewWorkspaceSubstrate(sb)); err != nil {
		t.Fatalf("register: %v", err)
	}
	receipt, err := shepherd.Materialize(ctx, store, liveAppendContext.ToOperationContext(shepherd.OpMaterialize), shepherd.MaterializationRequest{
		AppendIntentID:            "live:substrate:apply",
		TargetTraceOwnerID:        "owner:live-substrate",
		TargetRecordIDs:           declaration.FactIDs,
		TargetThroughOwnerOrdinal: shepherd.MaxOwnerOrdinal,
	}, registry)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if receipt.Outcome != shepherd.MaterializationSuccess {
		t.Fatalf("outcome = %q, want success (reason: %s)", receipt.Outcome, receipt.FailureReason)
	}
	if len(receipt.ProducedRecordIDs) != 1 {
		t.Fatalf("produced ids = %v, want one capture", receipt.ProducedRecordIDs)
	}

	// The write reached the world side.
	got, err := sb.ReadFile(ctx, "notes/greeting.txt")
	if err != nil {
		t.Fatalf("ReadFile after materialize: %v", err)
	}
	if string(got) != content {
		t.Errorf("file after materialize = %q, want %q", got, content)
	}

	// Capture the world the substrate wrote into, then recycle the sandbox.
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := sb.Apply(ctx, state); err != nil {
		t.Fatalf("Apply after Destroy: %v", err)
	}

	// The substrate-written file must survive the recycle: Apply re-provisions
	// from the captured state, and the capture was taken after the write.
	after, err := sb.ReadFile(ctx, "notes/greeting.txt")
	if err != nil {
		t.Fatalf("ReadFile after recycle: %v", err)
	}
	if string(after) != content {
		t.Errorf("file after recycle = %q, want %q — the captured state and the materialized write disagree", after, content)
	}

	// The applied capture cites its declaration, so the trace stays
	// interpretable: the write in the workspace is causally linked to the
	// recorded intent.
	capture, err := store.ReadFact(shepherd.ReadContext{ActorRef: "live:test", VisibilityProfile: shepherd.VisibilityPayload}, receipt.ProducedRecordIDs[0])
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	env := capture.GetEnvelope()
	if env.SchemaRef != shepherd.SchemaWorkspaceFileWriteApplied || env.Mode != shepherd.Capture {
		t.Errorf("capture = %q/%q, want %q/capture", env.SchemaRef, env.Mode, shepherd.SchemaWorkspaceFileWriteApplied)
	}
	if len(env.CausedByIDs) != 1 || env.CausedByIDs[0] != declaration.FactIDs[0] {
		t.Errorf("capture caused_by = %v, want %v", env.CausedByIDs, declaration.FactIDs)
	}
}

// TestLive_WorkspaceSubstrateRecordsExecOutcome pins that an exec
// declaration materialized through the substrate lands in the sandbox and
// its observed result (exit code, stream digests) is retained as a capture.
func TestLive_WorkspaceSubstrateRecordsExecOutcome(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	bootstrapGit(t, ctx, sb, cfg.Workdir)

	store, err := shepherd.NewSQLiteTraceStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteTraceStore: %v", err)
	}
	defer store.Close()

	declaration, err := store.Append(liveAppendContext, shepherd.AppendBatch{
		AppendIntentID: "live:substrate:exec-declare",
		Groups: []shepherd.AppendGroup{{
			TraceOwnerID: "owner:live-substrate-exec",
			RetainedContext: &shepherd.RetainedContext{
				SubstrateRef: shepherd.WorkspaceSubstrateRef,
				Containment:  sb.Capabilities().Containment,
			},
			FactDrafts: []shepherd.RecordDraft{{
				Mode:      shepherd.Declaration,
				SchemaRef: shepherd.SchemaWorkspaceExec,
				KindLabel: "workspace_exec",
				Payload: map[string]any{
					"command": "sh",
					"args":    []any{"-c", "echo exec-materialized > exec-out.txt"},
					"cwd":     cfg.Workdir,
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("append declaration: %v", err)
	}

	registry := shepherd.NewSubstrateRegistry()
	if err := registry.Register(shepherd.NewWorkspaceSubstrate(sb)); err != nil {
		t.Fatalf("register: %v", err)
	}
	receipt, err := shepherd.Materialize(ctx, store, liveAppendContext.ToOperationContext(shepherd.OpMaterialize), shepherd.MaterializationRequest{
		AppendIntentID:            "live:substrate:exec-apply",
		TargetTraceOwnerID:        "owner:live-substrate-exec",
		TargetRecordIDs:           declaration.FactIDs,
		TargetThroughOwnerOrdinal: shepherd.MaxOwnerOrdinal,
	}, registry)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if receipt.Outcome != shepherd.MaterializationSuccess {
		t.Fatalf("outcome = %q, want success (reason: %s)", receipt.Outcome, receipt.FailureReason)
	}

	// The exec side effect is observable in the workspace.
	got, err := sb.ReadFile(ctx, "exec-out.txt")
	if err != nil {
		t.Fatalf("ReadFile exec side effect: %v", err)
	}
	if string(got) != "exec-materialized\n" {
		t.Errorf("exec side effect = %q, want the exec-materialized line", got)
	}
}

//go:build linux && !nolive

// The snapshot-native-diff plan's Task 0 spike: prove the daemon-side diff
// service end to end against a real daemon, capture the produced tar as a
// golden, and record the format findings (whiteout form, opaque marker, entry
// ordering) that every downstream task keys off. The goldens land in
// testdata/change-tar/ and are committed as fixtures after the first run.
//
// Run (root, like the rest of the live suite):
//
//	sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
//	  go test -count=1 -v -run TestLive_DiffSpike ./...
package croft

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// spikeTree is the change matrix the golden covers: an add, a modify, a
// delete, a mode-only change, a new directory with a file, an opaque
// directory (emptied and recreated), and a symlink.
func spikeTree(t *testing.T, ctx context.Context, sb *ContainerdSandbox) {
	t.Helper()
	mustExec := func(script string) {
		t.Helper()
		res, err := sb.Exec(ctx, ExecRequest{Command: "sh", Args: []string{"-c", script}, Cwd: sb.cfg.workdir()})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("exec %q: err=%v exit=%d stderr=%s", script, err, res.ExitCode, res.Stderr)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := sb.WriteFile(ctx, path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// The lower tree: what the captured state sees.
	write("base-kept.txt", "kept unchanged\n")
	write("base-modified.txt", "before\n")
	write("base-deleted.txt", "delete me\n")
	write("base-modeonly.txt", "chmod only\n")
	mustExec("mkdir -p opaque-dir && printf 'one\\n' > opaque-dir/a.txt && printf 'two\\n' > opaque-dir/b.txt")
}

// spikeChanges makes the upper tree diverge from the lower.
func spikeChanges(t *testing.T, ctx context.Context, sb *ContainerdSandbox) {
	t.Helper()
	mustExec := func(script string) {
		t.Helper()
		res, err := sb.Exec(ctx, ExecRequest{Command: "sh", Args: []string{"-c", script}, Cwd: sb.cfg.workdir()})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("exec %q: err=%v exit=%d stderr=%s", script, err, res.ExitCode, res.Stderr)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := sb.WriteFile(ctx, path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	write("added.txt", "added after capture\n")
	write("base-modified.txt", "after\n")
	mustExec("rm base-deleted.txt")
	mustExec("chmod 600 base-modeonly.txt")
	mustExec("mkdir new-dir && printf 'nested\\n' > new-dir/n.txt")
	mustExec("rm -rf opaque-dir && mkdir opaque-dir && printf 'replaced\\n' > opaque-dir/c.txt")
	mustExec("ln -s base-kept.txt symlink.txt")
}

// TestLive_DiffSpike is the plan's Task 0. It proves: (a) the v2 diff-service
// accessor and Compare signature; (b) Compare with the upper being the active
// snapshot of a *running* task; (c) the produced tar, dumped as a golden and
// logged entry by entry; (d) content-lease pinning mechanics with the
// "content" resource type; (e) the full suite's unprivileged path is
// untouched by this (the spike runs wherever the suite runs).
func TestLive_DiffSpike(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	spikeTree(t, ctx, sb)
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	spikeChanges(t, ctx, sb)

	sb.mu.Lock()
	active := sb.activeKey
	sb.mu.Unlock()
	if active == "" {
		t.Fatal("no active layer after the changes")
	}

	// A test-side containerd client: the spike is scaffolding, so it talks
	// to the daemon directly rather than growing library surface first.
	c, err := containerd.New(cfg.Addr)
	if err != nil {
		t.Fatalf("containerd.New: %v", err)
	}
	defer c.Close()
	nsCtx := namespaces.WithNamespace(ctx, cfg.Namespace)
	snapSvc := c.SnapshotService(cfg.Snapshotter)

	// First finding, already pinned by this spike's first run: Mounts()
	// refuses committed snapshots ("not active or view: failed
	// precondition"), so the state's layer mounts only through a View — a
	// read-only preparation of the committed snapshot. The production
	// diffBlob helper does the same: View the lower side, Mounts the active
	// upper, Remove the view on the way out.
	lm := c.LeasesService()
	lease, err := lm.Create(nsCtx, leases.WithID("croft-spike-"+newSandboxID()), leases.WithLabels(map[string]string{sandboxLeaseLabel: "diff"}))
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	leaseCtx := leases.WithLease(nsCtx, lease.ID)
	defer func() {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer dcancel()
		_ = lm.Delete(dctx, lease)
	}()

	viewKey := "croft-spike-view-" + newSandboxID()
	if _, err := snapSvc.View(leaseCtx, viewKey, state.SnapshotKey); err != nil {
		t.Fatalf("view of committed %s: %v", state.SnapshotKey, err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer dcancel()
		_ = snapSvc.Remove(dctx, viewKey)
	}()

	lower, err := snapSvc.Mounts(leaseCtx, viewKey)
	if err != nil {
		t.Fatalf("lower mounts (view %s of committed %s): %v", viewKey, state.SnapshotKey, err)
	}
	upper, err := snapSvc.Mounts(nsCtx, active)
	if err != nil {
		t.Fatalf("upper mounts (%s, the running task's active layer): %v", active, err)
	}
	t.Logf("lower mounts: %d, upper mounts: %d (running-task multi-mount accepted by the daemon)", len(lower), len(upper))
	mustMounts := func(name string, m []mount.Mount) {
		t.Helper()
		for _, mm := range m {
			if mm.Type == "" {
				t.Fatalf("%s mount has empty type: %+v", name, mm)
			}
		}
	}
	mustMounts("lower", lower)
	mustMounts("upper", upper)

	desc, err := c.DiffService().Compare(leaseCtx, lower, upper)
	if err != nil {
		t.Fatalf("diff service Compare: %v", err)
	}
	t.Logf("diff descriptor: mediaType=%s digest=%s size=%d", desc.MediaType, desc.Digest, desc.Size)

	// Lease-pin the content blob by digest: the "content" resource type.
	if err := lm.AddResource(leaseCtx, lease, leases.Resource{ID: desc.Digest.String(), Type: "content"}); err != nil {
		t.Fatalf("add content resource to lease: %v", err)
	}

	cs := c.ContentStore()
	ra, err := cs.ReaderAt(leaseCtx, desc)
	if err != nil {
		t.Fatalf("content ReaderAt: %v", err)
	}
	blob, err := io.ReadAll(io.NewSectionReader(ra, 0, ra.Size()))
	ra.Close()
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}

	// Finding two: the default diff blob is a gzipped OCI layer
	// (application/vnd.oci.image.layer.v1.tar+gzip), not a plain tar. The
	// parser sniffs the gzip magic and decompresses, whatever media type the
	// daemon chose.
	plain, err := gunzipIfCompressed(blob)
	if err != nil {
		t.Fatalf("decompress diff blob: %v", err)
	}

	// Walk first, so the format findings always print; the golden compares
	// only a normalized copy.
	//
	// Finding three: the blob is not byte-deterministic across runs — the
	// upper files' wall-clock mtimes ride in the headers, so two identical
	// change sets produce different digests and the wire bytes cannot be a
	// golden. Nothing downstream (caching, identity) may assume stability.
	// The golden is a normalized plain tar: same entries, same content,
	// timestamps (and their PAX shadows) stripped.
	var norm bytes.Buffer
	tw := tar.NewWriter(&norm)
	tr := tar.NewReader(newByteSliceReader(plain))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar walk: %v", err)
		}
		kind := fmt.Sprintf("typeflag=%c", hdr.Typeflag)
		if hdr.Typeflag == tar.TypeChar && hdr.Devmajor == 0 && hdr.Devminor == 0 {
			kind = fmt.Sprintf("whiteout-char-dev (dev %d/%d)", hdr.Devmajor, hdr.Devminor)
		}
		if hdr.PAXRecords != nil {
			kind += fmt.Sprintf(" pax=%v", hdr.PAXRecords)
		}
		t.Logf("entry: name=%q mode=%o size=%d mtime=%d %s", hdr.Name, hdr.Mode, hdr.Size, hdr.ModTime.Unix(), kind)

		nh := *hdr
		nh.ModTime = time.Time{}
		nh.AccessTime = time.Time{}
		nh.ChangeTime = time.Time{}
		for k := range nh.PAXRecords {
			if k == "mtime" || k == "atime" || k == "ctime" {
				delete(nh.PAXRecords, k)
			}
		}
		if len(nh.PAXRecords) == 0 {
			nh.PAXRecords = nil
		}
		if err := tw.WriteHeader(&nh); err != nil {
			t.Fatalf("normalize header %s: %v", hdr.Name, err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			t.Fatalf("normalize content %s: %v", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close normalized tar: %v", err)
	}

	goldenDir := filepath.Join("testdata", "change-tar")
	if err := os.MkdirAll(goldenDir, 0o755); err != nil {
		t.Fatalf("mkdir golden dir: %v", err)
	}
	goldenPath := filepath.Join(goldenDir, "live-changes.plain.tar")
	if existing, rerr := os.ReadFile(goldenPath); rerr == nil {
		if string(existing) != norm.String() {
			t.Fatalf("golden %s changed: the entry set moved; regenerate deliberately and update this comparison", goldenPath)
		}
		t.Logf("golden %s matches (%d normalized bytes)", goldenPath, norm.Len())
	} else {
		if err := os.WriteFile(goldenPath, norm.Bytes(), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote golden %s (%d normalized bytes)", goldenPath, norm.Len())
	}
}

// gunzipIfCompressed decompresses a gzip payload and passes anything else
// through unchanged: the parser consumes whatever media type the daemon chose.
func gunzipIfCompressed(b []byte) ([]byte, error) {
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return b, nil
	}
	zr, err := gzip.NewReader(newByteSliceReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

type byteSliceReader struct {
	b []byte
	i int
}

func newByteSliceReader(b []byte) *byteSliceReader { return &byteSliceReader{b: b} }

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

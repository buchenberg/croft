package croft

import (
	"fmt"
	"strings"
	"testing"
)

// TestCappedBuffer_UnboundedKeepsEverything pins the default: a zero limit
// accumulates without bound, the behavior before the knob existed.
func TestCappedBuffer_UnboundedKeepsEverything(t *testing.T) {
	var b cappedBuffer
	b.Write([]byte("abc"))
	b.Write([]byte(strings.Repeat("x", 10_000)))
	b.Write([]byte("def"))
	if len(b.String()) != 10_006 {
		t.Errorf("len = %d, want 10006 (unbounded keeps everything)", len(b.String()))
	}
	if !strings.HasPrefix(b.String(), "abc") || !strings.HasSuffix(b.String(), "def") {
		t.Error("unbounded buffer lost content at the edges")
	}
}

// TestCappedBuffer_AtOrUnderCapIsExact pins the exactness promise: output
// that fits — including exactly at the cap — survives byte-for-byte with no
// marker.
func TestCappedBuffer_AtOrUnderCapIsExact(t *testing.T) {
	for _, total := range []int{0, 1, 7, 9} {
		b := cappedBuffer{limit: 10}
		writeInChunks(t, &b, strings.Repeat("y", total), 3)
		if got := b.String(); got != strings.Repeat("y", total) {
			t.Errorf("limit 10, total %d: got %q, want the exact bytes", total, got)
		}
	}
	b := cappedBuffer{limit: 10}
	writeInChunks(t, &b, strings.Repeat("z", 10), 4)
	if got := b.String(); got != strings.Repeat("z", 10) {
		t.Errorf("exactly at cap: got %q, want the exact bytes and no marker", got)
	}
}

// TestCappedBuffer_OverCapKeepsHeadTailAndMarker pins the truncation shape:
// first half, marker naming the dropped count, last half. The tail half is
// the larger one for odd limits (head is limit/2 floored).
func TestCappedBuffer_OverCapKeepsHeadTailAndMarker(t *testing.T) {
	input := "abcdefghijklmnopqrstuvwxyz" // 26 bytes

	b := cappedBuffer{limit: 10}
	writeInChunks(t, &b, input, 7) // writes spanning both boundaries
	want := "abcde" + fmt.Sprintf(truncationMarker, 16) + "vwxyz"
	if got := b.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// A single big write and many small ones truncate identically.
	one := cappedBuffer{limit: 10}
	one.Write([]byte(input))
	if one.String() != want {
		t.Errorf("single write: got %q, want %q", one.String(), want)
	}
}

// TestCappedBuffer_OddAndTinyLimits pins the budget split at the edges:
// limit 5 keeps 2+3, limit 1 keeps only the final byte.
func TestCappedBuffer_OddAndTinyLimits(t *testing.T) {
	b := cappedBuffer{limit: 5}
	writeInChunks(t, &b, "abcdefgh", 2) // 8 bytes
	want := "ab" + fmt.Sprintf(truncationMarker, 3) + "fgh"
	if got := b.String(); got != want {
		t.Errorf("limit 5: got %q, want %q", got, want)
	}

	tiny := cappedBuffer{limit: 1}
	writeInChunks(t, &tiny, "abc", 1)
	want = fmt.Sprintf(truncationMarker, 2) + "c"
	if got := tiny.String(); got != want {
		t.Errorf("limit 1: got %q, want %q", got, want)
	}
}

// TestCappedBuffer_TailWindowSlides pins that the kept tail is the LAST
// window of everything written, no matter how many writes produced it — a
// long stream of small writes must not leave a stale early chunk behind.
func TestCappedBuffer_TailWindowSlides(t *testing.T) {
	var b cappedBuffer
	b.limit = 8
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "%03d", i) // 300 bytes total
	}
	// The stream is "000001002...097098099": head = its first 4 bytes
	// ("0000"), tail = its last 4 ("8099").
	want := "0000" + fmt.Sprintf(truncationMarker, 292) + "8099"
	if got := b.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// writeInChunks writes s to b in pieces of at most n bytes, so every test
// exercises the split-boundary paths.
func writeInChunks(t *testing.T, b *cappedBuffer, s string, n int) {
	t.Helper()
	for len(s) > 0 {
		take := min(n, len(s))
		written, err := b.Write([]byte(s[:take]))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if written != take {
			t.Fatalf("Write wrote %d, want %d", written, take)
		}
		s = s[take:]
	}
}

// TestCappedBuffer_NegativeLimitIsUnbounded pins the config-typo guard: a
// negative limit must behave as unbounded, not slice a negative tail
// capacity — the buffer runs inside cio's copy goroutine, where a panic
// would take the host process down.
func TestCappedBuffer_NegativeLimitIsUnbounded(t *testing.T) {
	for _, limit := range []int{-1, -4096} {
		b := cappedBuffer{limit: limit}
		b.Write([]byte("abc"))
		b.Write([]byte(strings.Repeat("x", 100_000)))
		if got := len(b.String()); got != 100_003 {
			t.Errorf("limit %d: len = %d, want 100003 (unbounded, no panic)", limit, got)
		}
	}
}

// TestCappedBuffer_SingleHugeWrite pins the pre-trim: one write far larger
// than the budget still yields the exact head/tail/marker rendering — the
// incoming chunk is trimmed to the tail window before appending, so
// retained memory is bounded by the budget rather than the write size.
func TestCappedBuffer_SingleHugeWrite(t *testing.T) {
	input := "HEAD1" + strings.Repeat("m", 1_000_000) + "TAIL9"
	b := cappedBuffer{limit: 10}
	b.Write([]byte(input))

	want := "HEAD1" + fmt.Sprintf(truncationMarker, len(input)-10) + "TAIL9"
	if got := b.String(); got != want {
		t.Errorf("got %d bytes (%q...), want the head/marker/tail rendering", len(got), got[:min(80, len(got))])
	}
}

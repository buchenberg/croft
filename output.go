package croft

import (
	"bytes"
	"fmt"
)

// truncationMarker names the dropped byte count between the kept halves of
// an over-cap stream: a host debugging a truncated exec sees how much is
// missing, not a silent loss.
const truncationMarker = "[... %d bytes truncated ...]"

// cappedBuffer collects one exec stream (stdout or stderr) under the
// Config.MaxOutputBytes ceiling. The cap applies per stream, independently,
// and only on the public Exec path: internal protocol reads (ReadFile's cat,
// the diff reader tasks) are data transport, not diagnostics, and stay
// lossless — see ExecRequest.capOutput.
//
// Under the cap it is exact: every byte survives, in order. Over the cap it
// keeps the first limit/2 bytes and the LAST limit-limit/2 bytes with the
// truncation marker between them — heads show the command's banner, tails
// show the actual error, which is what an agent reads a failed exec for.
// Keeping only a prefix would discard exactly the part that matters.
//
// The cut is byte-based: a UTF-8 sequence or a line may split at either
// boundary. Documented, not corrected — output is diagnostic text, and
// rune-aware trimming would make the kept size nondeterministic.
//
// A non-positive limit means unbounded: a plain accumulation, the behavior
// before this knob existed and the default that changes nothing. Negative
// values are treated as unbounded rather than rejected or panic-sliced —
// the buffer runs inside cio's copy goroutine, where a panic would take the
// host process down for what is a config typo.
type cappedBuffer struct {
	limit int

	// Unbounded mode (limit <= 0).
	unbounded bytes.Buffer

	// Bounded mode: head fills first (limit/2 bytes), then tail keeps a
	// trailing window of the remaining budget, trimmed from the front as
	// it overflows.
	head  []byte
	tail  []byte
	total int64
}

// Write implements io.Writer. It never fails and never blocks: the ceiling
// is on memory, not on the writer.
func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += int64(n)
	if b.limit <= 0 {
		return b.unbounded.Write(p)
	}

	headRoom := b.limit / 2
	if room := headRoom - len(b.head); room > 0 {
		take := min(room, n)
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > 0 {
		tailCap := b.limit - headRoom
		// Trim the incoming write first: only its last tailCap bytes can
		// survive the window anyway, and appending the whole write would
		// retain a backing allocation the size of the write — a single
		// 32 KiB chunk against a 1 KiB cap must not allocate 31 KiB of
		// doomed tail. Retained memory stays bounded by the budget, not
		// by the write size.
		if len(p) > tailCap {
			p = p[len(p)-tailCap:]
		}
		b.tail = append(b.tail, p...)
		if len(b.tail) > tailCap {
			b.tail = b.tail[len(b.tail)-tailCap:]
		}
	}
	return n, nil
}

// String renders the stream: every byte when it fit under the cap,
// otherwise the kept head and tail around the truncation marker.
func (b *cappedBuffer) String() string {
	if b.limit <= 0 {
		return b.unbounded.String()
	}
	if b.total <= int64(b.limit) {
		return string(b.head) + string(b.tail)
	}
	dropped := b.total - int64(len(b.head)) - int64(len(b.tail))
	return string(b.head) + fmt.Sprintf(truncationMarker, dropped) + string(b.tail)
}

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
// Config.MaxOutputBytes ceiling. The cap applies per stream, independently.
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
// Zero limit means unbounded: a plain accumulation, the behavior before this
// knob existed and the default that changes nothing.
type cappedBuffer struct {
	limit int

	// Unbounded mode (limit == 0).
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
	if b.limit == 0 {
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
	if b.limit == 0 {
		return b.unbounded.String()
	}
	if b.total <= int64(b.limit) {
		return string(b.head) + string(b.tail)
	}
	dropped := b.total - int64(len(b.head)) - int64(len(b.tail))
	return string(b.head) + fmt.Sprintf(truncationMarker, dropped) + string(b.tail)
}

package daemon

import (
	"context"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// The subscriber mailbox is the single owner of run-event overflow policy.
//
// The pipeline executor publishes into it and must never be stalled by a slow,
// wedged, or dead subscriber, so every publish path is non-blocking and the
// mailbox is strictly bounded. Boundedness means something has to give under
// pressure; the mailbox decides what, using the ipc.EventClass taxonomy:
//
//   - ClassActivity (log chunks) is droppable, and is the ONLY class ever
//     dropped or evicted. Losing a log line cannot make a consumer render a
//     state the daemon no longer holds.
//   - ClassState is never silently lost. It is either delivered, or folded
//     into a single sticky, coalescing gap signal that forces the subscriber
//     to read authoritative state once. Folding is safe because every state
//     event's payload is reconstructable from get_run.
//
// Why a coalescing scalar and not a reserved slot: with a fixed bound, any
// scheme that tries to preserve N distinct must-not-lose payloads fails at
// N+1. A gap is one boolean and one high-water revision, so any number of
// simultaneous transitions collapse into it.
//
// Why a ring under a mutex and not a buffered channel: a channel cannot be
// evicted from the producer side without racing the reader for the same slot,
// and a ring gives exact count AND byte accounting for the bound.
const (
	// mailboxMaxEvents bounds queued events per subscriber. It matches the
	// historical 64-slot subscriber channel, so behaviour under normal load
	// is unchanged.
	mailboxMaxEvents = 64
	// mailboxMaxBytes bounds queued payload per subscriber independently of
	// the count, so a burst of large frames cannot inflate a mailbox.
	mailboxMaxBytes = 1 << 20
	// eventOverheadBytes approximates fixed fields and JSON framing so the
	// byte bound accounts for small frames too.
	eventOverheadBytes = 128
)

type eventMailbox struct {
	runID string

	mu     sync.Mutex
	ring   []ipc.Event
	bytes  int
	gap    bool
	gapRev int64
	closed bool

	// Observability only; never affects delivery decisions.
	droppedActivity uint64
	coalescedState  uint64

	// signal is a capacity-1, data-free wakeup, so a publisher can always
	// ping a waiting reader without blocking.
	signal chan struct{}
}

// newEventMailbox creates a mailbox that already holds a gap at startRev.
// Every subscription therefore opens with a reconcile trigger, which makes
// "subscribe, then read authoritative state" a server-side invariant instead
// of a rule each consumer has to remember, and makes reconnect converge.
func newEventMailbox(runID string, startRev int64) *eventMailbox {
	return &eventMailbox{
		runID:  runID,
		ring:   make([]ipc.Event, 0, mailboxMaxEvents),
		signal: make(chan struct{}, 1),
		gap:    true,
		gapRev: startRev,
	}
}

// eventBytes is the mailbox's accounting size for one frame. It counts the
// variable-length payload plus a fixed allowance for the rest.
func eventBytes(e ipc.Event) int {
	n := eventOverheadBytes
	if e.Content != nil {
		n += len(*e.Content)
	}
	if e.Findings != nil {
		n += len(*e.Findings)
	}
	if e.Error != nil {
		n += len(*e.Error)
	}
	return n
}

// publish enqueues one frame. It never blocks and never fails: the caller is
// the pipeline executor, which has nothing useful to do with an error.
func (mb *eventMailbox) publish(e ipc.Event) {
	mb.mu.Lock()
	if mb.closed {
		mb.mu.Unlock()
		return
	}
	size := eventBytes(e)
	if ipc.ClassOf(e.Type) == ipc.ClassActivity {
		if mb.hasRoomLocked(size) {
			mb.ring = append(mb.ring, e)
			mb.bytes += size
		} else {
			// Drop the newest. Dropping the oldest would rewrite log history
			// the consumer has not read yet; this keeps a contiguous prefix.
			mb.droppedActivity++
		}
		mb.mu.Unlock()
		mb.ping()
		return
	}

	// State (and any unrecognised type, which fails safe to state).
	switch {
	case mb.gap:
		// A gap is already outstanding. Queuing a delta behind it would let
		// a pre-snapshot state regress the post-snapshot render, so fold it
		// in instead. This is what keeps the signal coalescing.
		mb.foldLocked(e)
	default:
		if !mb.hasRoomLocked(size) {
			mb.evictActivityLocked(size)
		}
		if mb.hasRoomLocked(size) {
			mb.ring = append(mb.ring, e)
			mb.bytes += size
		} else {
			mb.foldLocked(e)
		}
	}
	mb.mu.Unlock()
	mb.ping()
}

func (mb *eventMailbox) foldLocked(e ipc.Event) {
	mb.gap = true
	mb.coalescedState++
	if e.StateRev > mb.gapRev {
		mb.gapRev = e.StateRev
	}
}

func (mb *eventMailbox) hasRoomLocked(size int) bool {
	return len(mb.ring) < mailboxMaxEvents && mb.bytes+size <= mailboxMaxBytes
}

// evictActivityLocked frees room for a state frame by discarding queued
// activity, oldest first. Only ClassActivity is ever evicted; a ring holding
// nothing but state is left untouched and the caller folds into the gap.
func (mb *eventMailbox) evictActivityLocked(size int) {
	for i := 0; i < len(mb.ring); {
		if ipc.ClassOf(mb.ring[i].Type) != ipc.ClassActivity {
			i++
			continue
		}
		mb.bytes -= eventBytes(mb.ring[i])
		mb.ring = append(mb.ring[:i], mb.ring[i+1:]...)
		mb.droppedActivity++
		if mb.hasRoomLocked(size) {
			return
		}
	}
}

func (mb *eventMailbox) ping() {
	select {
	case mb.signal <- struct{}{}:
	default:
	}
}

// close is a soft close: queued frames and any pending gap still drain, and
// only then does next report the stream finished. A hard close would discard a
// terminal transition that had folded into the gap.
func (mb *eventMailbox) close() {
	mb.mu.Lock()
	mb.closed = true
	mb.mu.Unlock()
	mb.ping()
}

// release drops retained payload once the subscription is gone, so a dangling
// reference cannot pin run snapshots.
func (mb *eventMailbox) release() {
	mb.mu.Lock()
	mb.ring = nil
	mb.bytes = 0
	mb.gap = false
	mb.gapRev = 0
	mb.closed = true
	mb.mu.Unlock()
	mb.ping()
}

// next returns the subscriber's next frame. ok is false once the stream is
// finished (soft-closed and fully drained) or ctx is done.
func (mb *eventMailbox) next(ctx context.Context) (ipc.Event, bool) {
	for {
		mb.mu.Lock()
		if mb.gap {
			mb.gap = false
			rev := mb.gapRev
			mb.gapRev = 0
			mb.mu.Unlock()
			// The gap drains ahead of queued payload on purpose: a coalesced
			// invalidation must reach the consumer before it renders more
			// stale frames, and the revision guard makes the deltas queued
			// behind it harmless no-ops.
			return ipc.Event{Type: ipc.EventStreamGap, RunID: mb.runID, StateRev: rev}, true
		}
		if len(mb.ring) > 0 {
			e := mb.ring[0]
			mb.ring = mb.ring[1:]
			mb.bytes -= eventBytes(e)
			mb.mu.Unlock()
			return e, true
		}
		closed := mb.closed
		mb.mu.Unlock()
		if closed {
			return ipc.Event{}, false
		}
		select {
		case <-mb.signal:
		case <-ctx.Done():
			return ipc.Event{}, false
		}
	}
}

// stats exposes mailbox occupancy for tests and diagnostics.
func (mb *eventMailbox) stats() (queued, queuedBytes int, gapPending bool, droppedActivity, coalescedState uint64) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return len(mb.ring), mb.bytes, mb.gap, mb.droppedActivity, mb.coalescedState
}

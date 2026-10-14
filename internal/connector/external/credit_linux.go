//go:build linux

package external

import (
	"context"
	"errors"
	"sync"

	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
)

const (
	maxQueueFramesPerInvocation = 64
	maxQueueBytesPerInvocation  = 4 << 20
	maxQueueFramesProcess       = 256
	maxQueueBytesProcess        = 16 << 20
	maxCreditFrameBytes         = 64 << 10
)

var (
	errCreditIdentity = errors.New("invalid or stale IPC credit identity")
	errCreditReplay   = errors.New("replayed or unreserved IPC data frame")
	errCreditLimit    = errors.New("IPC credit reservation exceeds limits")
)

type creditIdentity struct {
	epoch        uint64
	invocationID string
	operation    string
	callID       uint64
	direction    string
	sequence     uint64
}

func creditIdentityFromWire(w *gen.FlowIdentity) (creditIdentity, error) {
	if w == nil {
		return creditIdentity{}, errCreditIdentity
	}
	id := creditIdentity{epoch: w.Epoch, invocationID: w.InvocationId, operation: w.Operation, callID: w.CallId, direction: w.Direction, sequence: w.Sequence}
	if !id.valid() {
		return creditIdentity{}, errCreditIdentity
	}
	return id, nil
}

func (id creditIdentity) valid() bool {
	return id.epoch != 0 && id.invocationID != "" && (id.operation == "Execute" || id.operation == "HTTPDo") && id.callID != 0 && (id.direction == "host_to_child" || id.direction == "child_to_host") && id.sequence != 0
}

func (id creditIdentity) wire() *gen.FlowIdentity {
	return &gen.FlowIdentity{Epoch: id.epoch, InvocationId: id.invocationID, Operation: id.operation, CallId: id.callID, Direction: id.direction, Sequence: id.sequence}
}

type creditStream struct {
	invocationID string
	operation    string
	callID       uint64
	direction    string
}

type creditReservation struct {
	bytes     int
	delivered bool
}

type invocationCreditUsage struct {
	frames       int
	bytes        int
	reservations map[creditIdentity]creditReservation
	lastSequence map[creditStream]uint64
}

// creditBroker is host-owned. Reservations count once from grant until the
// receiver ACKs downstream delivery; revoke never waits for a data consumer.
type creditBroker struct {
	mu      sync.Mutex
	epoch   uint64
	active  map[string]*invocationCreditUsage
	frames  int
	bytes   int
	changed chan struct{}
	closed  bool
}

func newCreditBroker(epoch uint64) *creditBroker {
	return &creditBroker{epoch: epoch, active: make(map[string]*invocationCreditUsage), changed: make(chan struct{})}
}

func (b *creditBroker) register(invocationID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || invocationID == "" || b.active[invocationID] != nil {
		return errCreditIdentity
	}
	b.active[invocationID] = &invocationCreditUsage{reservations: make(map[creditIdentity]creditReservation), lastSequence: make(map[creditStream]uint64)}
	return nil
}

func (b *creditBroker) reserve(ctx context.Context, id creditIdentity, applicationBytes int) error {
	if !id.valid() || id.epoch != b.epoch || applicationBytes < 0 || applicationBytes > maxCreditFrameBytes {
		return errCreditIdentity
	}
	stream := creditStream{invocationID: id.invocationID, operation: id.operation, callID: id.callID, direction: id.direction}
	for {
		b.mu.Lock()
		usage := b.active[id.invocationID]
		if b.closed || usage == nil || id.sequence != usage.lastSequence[stream]+1 {
			b.mu.Unlock()
			return errCreditReplay
		}
		if _, exists := usage.reservations[id]; exists {
			b.mu.Unlock()
			return errCreditReplay
		}
		if usage.frames < maxQueueFramesPerInvocation && usage.bytes+applicationBytes <= maxQueueBytesPerInvocation && b.frames < maxQueueFramesProcess && b.bytes+applicationBytes <= maxQueueBytesProcess {
			usage.lastSequence[stream] = id.sequence
			usage.reservations[id] = creditReservation{bytes: applicationBytes}
			usage.frames++
			usage.bytes += applicationBytes
			b.frames++
			b.bytes += applicationBytes
			b.mu.Unlock()
			return nil
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (b *creditBroker) delivered(id creditIdentity, applicationBytes int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	usage := b.active[id.invocationID]
	reservation, ok := usageReservation(usage, id)
	if id.epoch != b.epoch || !ok || reservation.delivered || applicationBytes < 0 || applicationBytes != reservation.bytes {
		return errCreditReplay
	}
	reservation.delivered = true
	usage.reservations[id] = reservation
	return nil
}

func usageReservation(usage *invocationCreditUsage, id creditIdentity) (creditReservation, bool) {
	if usage == nil {
		return creditReservation{}, false
	}
	r, ok := usage.reservations[id]
	return r, ok
}

func (b *creditBroker) ack(identities []creditIdentity) error {
	if len(identities) == 0 {
		return errCreditIdentity
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, id := range identities {
		usage := b.active[id.invocationID]
		reservation, ok := usageReservation(usage, id)
		if id.epoch != b.epoch || !ok || (!reservation.delivered && id.direction != "host_to_child") {
			return errCreditReplay
		}
		for _, prior := range identities[:i] {
			if prior == id {
				return errCreditReplay
			}
		}
	}
	for _, id := range identities {
		usage := b.active[id.invocationID]
		reservation := usage.reservations[id]
		delete(usage.reservations, id)
		usage.frames--
		usage.bytes -= reservation.bytes
		b.frames--
		b.bytes -= reservation.bytes
	}
	b.notifyLocked()
	return nil
}

func (b *creditBroker) abandon(id creditIdentity) {
	b.mu.Lock()
	defer b.mu.Unlock()
	usage := b.active[id.invocationID]
	reservation, ok := usageReservation(usage, id)
	if !ok {
		return
	}
	delete(usage.reservations, id)
	usage.frames--
	usage.bytes -= reservation.bytes
	b.frames--
	b.bytes -= reservation.bytes
	b.notifyLocked()
}

func (b *creditBroker) revoke(invocationID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	usage := b.active[invocationID]
	if usage == nil {
		return
	}
	for _, reservation := range usage.reservations {
		b.frames--
		b.bytes -= reservation.bytes
	}
	delete(b.active, invocationID)
	b.notifyLocked()
}

func (b *creditBroker) revokeStream(invocationID, operation string, callID uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	usage := b.active[invocationID]
	if usage == nil {
		return
	}
	for id, reservation := range usage.reservations {
		if id.operation == operation && id.callID == callID {
			delete(usage.reservations, id)
			usage.frames--
			usage.bytes -= reservation.bytes
			b.frames--
			b.bytes -= reservation.bytes
		}
	}
	for key := range usage.lastSequence {
		if key.operation == operation && key.callID == callID {
			delete(usage.lastSequence, key)
		}
	}
	b.notifyLocked()
}

func (b *creditBroker) hasStream(invocationID, operation string, callID uint64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	usage := b.active[invocationID]
	if usage == nil {
		return false
	}
	for id := range usage.reservations {
		if id.operation == operation && id.callID == callID {
			return true
		}
	}
	return false
}

func (b *creditBroker) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	clear(b.active)
	b.frames, b.bytes = 0, 0
	b.notifyLocked()
}

func (b *creditBroker) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

func (b *creditBroker) usage() (frames, bytes int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.frames, b.bytes
}

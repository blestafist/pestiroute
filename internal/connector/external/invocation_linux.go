//go:build linux

package external

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
)

const (
	maxInvocations        = 4
	maxCallbacksPerInvoke = 8
	maxCallbacksProcess   = 32
)

var (
	errInvocationLimit = errors.New("external connector invocation limit reached")
	errCallbackLimit   = errors.New("external connector callback limit reached")
	errInvocationStale = errors.New("external connector invocation is stale or unknown")
	errCallbackReplay  = errors.New("external connector callback ID is duplicate or unknown")
)

// invocationRegistry is host-owned authority for one process epoch. Child data
// never selects an account; callbacks retrieve it through their admitted call.
type invocationRegistry struct {
	mu          sync.Mutex
	epoch       uint64
	active      map[string]*invocation
	calls       map[uint64]*invocation
	outstanding int
	closed      bool
	lastCallID  uint64
}

type invocation struct {
	id        string
	epoch     uint64
	accountID string
	ctx       context.Context
	cancel    context.CancelFunc
	callbacks map[uint64]struct{}
	revoked   bool
}

type callbackLease struct {
	registry   *invocationRegistry
	invocation *invocation
	callID     uint64
}

func newInvocationRegistry(epoch uint64) *invocationRegistry {
	return &invocationRegistry{epoch: epoch, active: make(map[string]*invocation), calls: make(map[uint64]*invocation)}
}

func (r *invocationRegistry) begin(parent context.Context, accountID string) (string, context.Context, error) {
	if err := parent.Err(); err != nil {
		return "", nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.active) >= maxInvocations {
		return "", nil, errInvocationLimit
	}
	if r.closed {
		return "", nil, errInvocationStale
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, err
	}
	id := hex.EncodeToString(raw[:])
	ctx, cancel := context.WithCancel(parent)
	inv := &invocation{id: id, epoch: r.epoch, accountID: accountID, ctx: ctx, cancel: cancel, callbacks: make(map[uint64]struct{})}
	r.active[id] = inv
	go func() {
		<-ctx.Done()
		r.revoke(r.epoch, id)
	}()
	return id, ctx, nil
}

func (r *invocationRegistry) admitCallback(epoch uint64, invocationID string, callID uint64) (*callbackLease, string, context.Context, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inv := r.active[invocationID]
	if epoch != r.epoch || inv == nil || inv.epoch != epoch || inv.revoked {
		return nil, "", nil, errInvocationStale
	}
	if inv.ctx.Err() != nil {
		r.revokeLocked(inv)
		return nil, "", nil, errInvocationStale
	}
	if callID == 0 {
		return nil, "", nil, errCallbackReplay
	}
	if owner := r.calls[callID]; owner != nil && owner != inv {
		return nil, "", nil, errCallbackReplay
	}
	if callID <= r.lastCallID {
		return nil, "", nil, errCallbackReplay
	}
	if len(inv.callbacks) >= maxCallbacksPerInvoke || r.outstanding >= maxCallbacksProcess {
		return nil, "", nil, errCallbackLimit
	}
	inv.callbacks[callID] = struct{}{}
	r.calls[callID] = inv
	r.lastCallID = callID
	r.outstanding++
	return &callbackLease{registry: r, invocation: inv, callID: callID}, inv.accountID, inv.ctx, nil
}

func (l *callbackLease) release() {
	r := l.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	inv := l.invocation
	if inv.revoked || r.active[inv.id] != inv {
		return
	}
	if _, ok := inv.callbacks[l.callID]; ok {
		delete(inv.callbacks, l.callID)
		delete(r.calls, l.callID)
		r.outstanding--
	}
}

// live gates result delivery; canceled or revoked callback results are dropped.
func (l *callbackLease) live() bool {
	r := l.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	inv := l.invocation
	if inv.ctx.Err() != nil {
		r.revokeLocked(inv)
		return false
	}
	_, active := inv.callbacks[l.callID]
	return !inv.revoked && r.active[inv.id] == inv && active
}

// revoke interrupts callbacks and returns admission immediately; it never
// waits for callbacks blocked on their data transport to finish.
func (r *invocationRegistry) revoke(epoch uint64, invocationID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	inv := r.active[invocationID]
	if epoch != r.epoch || inv == nil || inv.revoked {
		return false
	}
	r.revokeLocked(inv)
	return true
}

func (r *invocationRegistry) revokeLocked(inv *invocation) {
	if inv.revoked {
		return
	}
	inv.revoked = true
	delete(r.active, inv.id)
	r.outstanding -= len(inv.callbacks)
	for callID := range inv.callbacks {
		delete(r.calls, callID)
	}
	clear(inv.callbacks)
	inv.cancel()
}

func (r *invocationRegistry) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for id, inv := range r.active {
		inv.revoked = true
		inv.cancel()
		delete(r.active, id)
		for callID := range inv.callbacks {
			delete(r.calls, callID)
		}
		clear(inv.callbacks)
	}
	r.outstanding = 0
}

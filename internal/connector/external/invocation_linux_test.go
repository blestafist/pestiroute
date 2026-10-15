//go:build linux

package external

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestM6InvocationConcurrentIsolation(t *testing.T) {
	r := newInvocationRegistry(41)
	defer r.close()
	type result struct{ id, account string }
	results := make(chan result, maxInvocations)
	var wg sync.WaitGroup
	for i := 0; i < maxInvocations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			account := string(rune('a' + i))
			id, _, err := r.begin(context.Background(), account)
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			results <- result{id, account}
		}(i)
	}
	wg.Wait()
	close(results)
	got := make([]result, 0, maxInvocations)
	for x := range results {
		got = append(got, x)
	}
	for i, x := range got {
		lease, account, _, err := r.admitCallback(41, x.id, uint64(i+1))
		if err != nil || account != x.account {
			t.Fatalf("callback authority leaked: account=%q want=%q err=%v", account, x.account, err)
		}
		lease.release()
	}
	if _, _, err := r.begin(context.Background(), "overflow"); !errors.Is(err, errInvocationLimit) {
		t.Fatalf("fifth invocation accepted: %v", err)
	}
}

func TestM6InvocationRejectsStaleEpoch(t *testing.T) {
	r := newInvocationRegistry(7)
	defer r.close()
	id, _, err := r.begin(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.admitCallback(6, id, 1); !errors.Is(err, errInvocationStale) {
		t.Fatalf("stale epoch accepted: %v", err)
	}
	if !r.revoke(7, id) || r.revoke(7, id) {
		t.Fatal("invocation revocation was not one-shot")
	}
	if _, _, _, err := r.admitCallback(7, id, 2); !errors.Is(err, errInvocationStale) {
		t.Fatalf("revoked invocation accepted: %v", err)
	}
	if _, _, _, err := r.admitCallback(7, "unknown-host-invocation", 3); !errors.Is(err, errInvocationStale) {
		t.Fatalf("unknown invocation accepted: %v", err)
	}
}

func TestM6InvocationRejectsCrossCall(t *testing.T) {
	r := newInvocationRegistry(9)
	defer r.close()
	a, _, _ := r.begin(context.Background(), "a")
	b, _, _ := r.begin(context.Background(), "b")
	lease, _, _, err := r.admitCallback(9, a, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	if _, _, _, err := r.admitCallback(9, b, 10); !errors.Is(err, errCallbackReplay) {
		t.Fatalf("unknown/cross-invocation callback accepted: %v", err)
	}
	lease.release()
	if _, _, _, err := r.admitCallback(9, a, 10); !errors.Is(err, errCallbackReplay) {
		t.Fatalf("duplicate completed call ID accepted: %v", err)
	}
}

func TestM6InvocationBoundsOutstanding(t *testing.T) {
	r := newInvocationRegistry(12)
	defer r.close()
	id, _, _ := r.begin(context.Background(), "acct")
	leases := make([]*callbackLease, 0, maxCallbacksPerInvoke)
	for i := 0; i < maxCallbacksPerInvoke; i++ {
		lease, _, _, err := r.admitCallback(12, id, uint64(i+1))
		if err != nil {
			t.Fatalf("callback %d: %v", i, err)
		}
		leases = append(leases, lease)
	}
	if _, _, _, err := r.admitCallback(12, id, 99); !errors.Is(err, errCallbackLimit) {
		t.Fatalf("ninth callback accepted: %v", err)
	}
	for _, lease := range leases {
		lease.release()
	}
	ids := []string{id}
	for i := 1; i < maxInvocations; i++ {
		next, _, err := r.begin(context.Background(), "acct")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, next)
	}
	for i := 0; i < maxCallbacksProcess; i++ {
		if _, _, _, err := r.admitCallback(12, ids[i%maxInvocations], uint64(100+i)); err != nil {
			t.Fatalf("process callback %d: %v", i, err)
		}
	}
	if r.outstanding != maxCallbacksProcess {
		t.Fatalf("process callback count=%d want=%d", r.outstanding, maxCallbacksProcess)
	}
	if _, _, _, err := r.admitCallback(12, ids[0], 1000); !errors.Is(err, errCallbackLimit) {
		t.Fatalf("33rd process callback accepted: %v", err)
	}
}

func TestM6InvocationCancelControlProgress(t *testing.T) {
	r := newInvocationRegistry(14)
	defer r.close()
	id, ctx, err := r.begin(context.Background(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxCallbacksPerInvoke; i++ {
		if _, _, _, err := r.admitCallback(14, id, uint64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan bool, 1)
	go func() { done <- r.revoke(14, id) }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("cancel could not revoke saturated invocation")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel waited for stalled data callbacks")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel invocation context")
	}
	if _, _, _, err := r.admitCallback(14, id, 99); !errors.Is(err, errInvocationStale) {
		t.Fatalf("callback admitted after cancellation: %v", err)
	}
	if r.outstanding != 0 {
		t.Fatalf("revocation retained callback admission: %d", r.outstanding)
	}
}

func TestM6InvocationParentCancellation(t *testing.T) {
	r := newInvocationRegistry(15)
	defer r.close()
	preCanceled, cancelPreCanceled := context.WithCancel(context.Background())
	cancelPreCanceled()
	if _, _, err := r.begin(preCanceled, "account"); !errors.Is(err, context.Canceled) {
		t.Fatalf("already-canceled parent admitted: %v", err)
	}
	ids := make([]string, maxInvocations)
	cancels := make([]context.CancelFunc, maxInvocations)
	leases := make([][]*callbackLease, maxInvocations)
	defer func() {
		for _, cancel := range cancels {
			if cancel != nil {
				cancel()
			}
		}
	}()
	for i := range maxInvocations {
		parent, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		id, _, err := r.begin(parent, "account")
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		for j := 0; j < maxCallbacksPerInvoke; j++ {
			lease, _, _, err := r.admitCallback(15, id, uint64(i*maxCallbacksPerInvoke+j+1))
			if err != nil {
				t.Fatal(err)
			}
			leases[i] = append(leases[i], lease)
		}
	}
	if r.outstanding != maxCallbacksProcess {
		t.Fatalf("setup outstanding=%d want=%d", r.outstanding, maxCallbacksProcess)
	}
	cancels[0]()
	if _, _, _, err := r.admitCallback(15, ids[0], 100); !errors.Is(err, errInvocationStale) {
		t.Fatalf("callback authority remained open after parent cancellation: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		revoked := r.active[ids[0]] == nil && r.outstanding == maxCallbacksProcess-maxCallbacksPerInvoke
		r.mu.Unlock()
		if revoked {
			break
		}
		time.Sleep(time.Millisecond)
	}
	r.mu.Lock()
	active, outstanding := len(r.active), r.outstanding
	r.mu.Unlock()
	if active != maxInvocations-1 || outstanding != maxCallbacksProcess-maxCallbacksPerInvoke {
		t.Fatalf("parent cancellation did not revoke promptly: active=%d callbacks=%d", active, outstanding)
	}
	for _, lease := range leases[0] {
		if lease.live() {
			t.Fatal("late callback result retained authority after parent cancellation")
		}
		lease.release()
	}
	if _, _, _, err := r.admitCallback(15, ids[0], 100); !errors.Is(err, errInvocationStale) {
		t.Fatalf("callback admitted after parent cancellation: %v", err)
	}
	if _, _, err := r.begin(context.Background(), "new-account"); err != nil {
		t.Fatalf("cancelled invocation did not return admission: %v", err)
	}
}

//go:build linux

package external

import (
	"bytes"
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type selectedCredentials struct{ account string }

func (c selectedCredentials) Get(_ context.Context, name string) ([]byte, error) {
	if name != "token" {
		return nil, errors.New("missing")
	}
	return append([]byte(c.account+"\x00\xff"), bytes.Repeat([]byte{0, 0xff, 'x'}, 30<<10)...), nil
}

type blockingCredentials struct{}

func (blockingCredentials) Get(ctx context.Context, _ string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func credentialTestServer(t *testing.T, registry *invocationRegistry) gen.HostServicesClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := newHostServicesServer(registry)
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///credential-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = listener.Close() })
	return gen.NewHostServicesClient(conn)
}

func TestM6CredentialSelectedAccountAndOpaqueBytes(t *testing.T) {
	r := newInvocationRegistry(23)
	defer r.close()
	a, _, err := r.beginWithCredentials(context.Background(), "acct-a", selectedCredentials{"acct-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := r.beginWithCredentials(context.Background(), "acct-b", selectedCredentials{"acct-b"})
	if err != nil {
		t.Fatal(err)
	}
	client := credentialTestServer(t, r)
	for _, tc := range []struct{ id, account string }{{a, "acct-a"}, {b, "acct-b"}} {
		got, err := NewCredentialClient(client, 23, tc.id).Get(context.Background(), "token")
		want := append([]byte(tc.account+"\x00\xff"), bytes.Repeat([]byte{0, 0xff, 'x'}, 30<<10)...)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("credential bytes=%v err=%v, want %v", got, err, want)
		}
	}
}

func TestM6CredentialDeniesExpiredUnknownAndPostClose(t *testing.T) {
	r := newInvocationRegistry(31)
	client := credentialTestServer(t, r)
	id, _, err := r.beginWithCredentials(context.Background(), "acct", selectedCredentials{"acct"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.revoke(31, id); !err {
		t.Fatal("failed to revoke invocation")
	}
	for _, tc := range []struct {
		epoch uint64
		id    string
	}{{31, id}, {30, id}, {31, "unknown"}} {
		if value, err := NewCredentialClient(client, tc.epoch, tc.id).Get(context.Background(), "token"); err == nil || value != nil {
			t.Fatalf("stale/unknown credential granted: %q, %v", value, err)
		}
	}
	open, _, err := r.beginWithCredentials(context.Background(), "acct", selectedCredentials{"acct"})
	if err != nil {
		t.Fatal(err)
	}
	r.close()
	if value, err := NewCredentialClient(client, 31, open).Get(context.Background(), "token"); err == nil || value != nil {
		t.Fatalf("post-close credential granted: %q, %v", value, err)
	}
}

func TestM6CredentialParentCancellation(t *testing.T) {
	r := newInvocationRegistry(41)
	parent, cancel := context.WithCancel(context.Background())
	id, _, err := r.beginWithCredentials(parent, "acct", selectedCredentials{"acct"})
	if err != nil {
		t.Fatal(err)
	}
	client := credentialTestServer(t, r)
	cancel()
	if value, err := NewCredentialClient(client, 41, id).Get(context.Background(), "token"); err == nil || value != nil {
		t.Fatalf("cancelled invocation credential granted: %q, %v", value, err)
	}
}

func TestM6CredentialExpiredInvocation(t *testing.T) {
	r := newInvocationRegistry(42)
	defer r.close()
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	id, _, err := r.beginWithCredentials(parent, "acct", selectedCredentials{"acct"})
	if err != nil {
		t.Fatal(err)
	}
	<-parent.Done()
	client := credentialTestServer(t, r)
	if value, err := NewCredentialClient(client, 42, id).Get(context.Background(), "token"); err == nil || value != nil {
		t.Fatalf("expired invocation credential granted: %q, %v", value, err)
	}
}

func TestM6CredentialRejectsDuplicateCallID(t *testing.T) {
	r := newInvocationRegistry(43)
	defer r.close()
	id, _, err := r.beginWithCredentials(context.Background(), "acct", selectedCredentials{"acct"})
	if err != nil {
		t.Fatal(err)
	}
	client := NewCredentialClient(credentialTestServer(t, r), 43, id)
	callID := credentialCallID.Add(1)
	if _, err := client.getWithCallID(context.Background(), "token", callID); err != nil {
		t.Fatal(err)
	}
	if value, err := client.getWithCallID(context.Background(), "token", callID); err == nil || value != nil {
		t.Fatalf("duplicate callback ID granted credentials: %q, %v", value, err)
	}
}

func TestM6CredentialAcceptsOutOfOrderUniqueCallIDs(t *testing.T) {
	r := newInvocationRegistry(44)
	defer r.close()
	id, _, err := r.beginWithCredentials(context.Background(), "acct", selectedCredentials{"acct"})
	if err != nil {
		t.Fatal(err)
	}
	client := NewCredentialClient(credentialTestServer(t, r), 44, id)
	for _, callID := range []uint64{2, 1} {
		if got, err := client.getWithCallID(context.Background(), "token", callID); err != nil || len(got) == 0 {
			t.Fatalf("unique reordered call ID %d rejected: bytes=%d err=%v", callID, len(got), err)
		}
	}
	for _, callID := range []uint64{1, 2} {
		if got, err := client.getWithCallID(context.Background(), "token", callID); err == nil || got != nil {
			t.Fatalf("reordered duplicate call ID %d accepted: bytes=%d err=%v", callID, len(got), err)
		}
	}
}

func TestM6CredentialTransportRejectsOversizedFrame(t *testing.T) {
	r := newInvocationRegistry(45)
	defer r.close()
	client := credentialTestServer(t, r)
	stream, err := client.CredentialGet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fragment := &gen.RecordFragment{Epoch: 45, CallId: 1, RecordId: 0, RecordType: "CredentialLookup", TotalLength: 1, Final: true, Data: make([]byte, ipc.MaxGRPCMessageBytes)}
	err = stream.Send(fragment)
	if err == nil {
		err = stream.CloseSend()
	}
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized gRPC frame was not rejected by transport: %v", err)
	}
}

func TestM6CredentialCallbackLimits(t *testing.T) {
	r := newInvocationRegistry(51)
	defer r.close()
	ids := make([]string, maxInvocations)
	for i := range ids {
		id, _, err := r.beginWithCredentials(context.Background(), "acct", selectedCredentials{"acct"})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	var leases []*callbackLease
	for _, id := range ids {
		for range maxCallbacksPerInvoke {
			callID := credentialCallID.Add(1)
			lease, _, _, err := r.admitCallback(51, id, callID)
			if err != nil {
				t.Fatal(err)
			}
			leases = append(leases, lease)
		}
	}
	defer func() {
		for _, lease := range leases {
			lease.release()
		}
	}()
	client := credentialTestServer(t, r)
	if value, err := NewCredentialClient(client, 51, ids[0]).Get(context.Background(), "token"); err == nil || value != nil {
		t.Fatalf("credential lookup bypassed 8/32 callback admission: %q, %v", value, err)
	}
}

func TestM6CredentialLookupTimeout(t *testing.T) {
	r := newInvocationRegistry(52)
	defer r.close()
	id, _, err := r.beginWithCredentials(context.Background(), "acct", blockingCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	client := credentialTestServer(t, r)
	started := time.Now()
	if value, err := NewCredentialClient(client, 52, id).Get(context.Background(), "token"); err == nil || value != nil || time.Since(started) > 3*time.Second {
		t.Fatalf("credential lookup timeout not enforced: value=%q err=%v elapsed=%v", value, err, time.Since(started))
	}
}

var _ core.CredentialAccess = selectedCredentials{}

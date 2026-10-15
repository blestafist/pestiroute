//go:build linux

package external

import (
	"context"
	"net"
	"testing"

	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestM6BackpressureCancelControlRejectsStaleUnknownAndReplay(t *testing.T) {
	const epoch = 91
	registry := newInvocationRegistry(epoch)
	invocationID, _, err := registry.begin(context.Background(), "account")
	if err != nil {
		t.Fatal(err)
	}
	lease, _, _, err := registry.admitCallback(epoch, invocationID, 41)
	if err != nil {
		t.Fatal(err)
	}
	dataID := creditIdentity{epoch: epoch, invocationID: invocationID, operation: "HTTPDo", callID: 41, direction: "child_to_host", sequence: 1}
	if err := registry.credits.reserve(context.Background(), dataID, 123); err != nil {
		t.Fatal(err)
	}

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	gen.RegisterHostServicesServer(server, &hostServices{registry: registry})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///flow-control-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := gen.NewHostServicesClient(conn)
	sendCancel := func(id creditIdentity) error {
		_, err := client.FlowControl(context.Background(), &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_CANCEL, Identity: id.wire()})
		return err
	}

	stale := dataID
	stale.epoch++
	if err := sendCancel(stale); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stale epoch cancel status=%v, want PermissionDenied", status.Code(err))
	}
	unknown := dataID
	unknown.callID++
	if err := sendCancel(unknown); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown call cancel status=%v, want PermissionDenied", status.Code(err))
	}
	controlID := dataID
	controlID.sequence = 1
	if err := sendCancel(controlID); err != nil {
		t.Fatalf("live call cancel failed: %v", err)
	}
	if frames, bytes := registry.credits.usage(); frames != 0 || bytes != 0 {
		t.Fatalf("cancel failed to revoke credits: (%d,%d)", frames, bytes)
	}
	if err := sendCancel(controlID); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("replayed cancel status=%v, want FailedPrecondition", status.Code(err))
	}
	lease.release()
	controlID.sequence++
	if err := sendCancel(controlID); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("closed call cancel status=%v, want PermissionDenied", status.Code(err))
	}
	registry.close()
}

//go:build linux

package external

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const credentialTimeout = 2 * time.Second

var credentialCallID atomic.Uint64

type hostServices struct {
	gen.UnimplementedHostServicesServer
	registry *invocationRegistry
}

type verifiedHostListener struct {
	net.Listener
	pid int
}

func (l verifiedHostListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		unix, ok := conn.(*net.UnixConn)
		if ok && VerifyPeer(unix, l.pid) == nil {
			return conn, nil
		}
		_ = conn.Close()
	}
}

func (p *Process) serveHostServices(ctx context.Context, registry *invocationRegistry) *grpc.Server {
	server := newHostServicesServer(registry)
	go func() {
		go func() {
			<-ctx.Done()
			server.Stop()
		}()
		_ = server.Serve(verifiedHostListener{Listener: p.listener, pid: p.PID()})
	}()
	return server
}

func newHostServicesServer(registry *invocationRegistry) *grpc.Server {
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(ipc.MaxGRPCMessageBytes),
		grpc.MaxSendMsgSize(ipc.MaxGRPCMessageBytes),
	)
	gen.RegisterHostServicesServer(server, &hostServices{registry: registry})
	return server
}

func (s *hostServices) CredentialGet(stream grpc.BidiStreamingServer[gen.RecordFragment, gen.RecordFragment]) error {
	requestCtx, requestCancel := context.WithTimeout(stream.Context(), credentialTimeout)
	defer requestCancel()
	first, err := recvCredentialFragment(requestCtx, stream)
	if err != nil {
		return status.Error(codes.InvalidArgument, "credential lookup denied")
	}
	id := ipc.Identity{Epoch: first.Epoch, CallID: first.CallId, RecordID: first.RecordId, Type: "CredentialLookup"}
	invocationID := first.InvocationId
	if invocationID == "" || id.RecordID != 0 {
		return status.Error(codes.PermissionDenied, "credential lookup denied")
	}
	lease, _, parent, err := s.registry.admitCallback(id.Epoch, invocationID, id.CallID)
	if err != nil {
		return status.Error(codes.PermissionDenied, "credential lookup denied")
	}
	defer lease.release()
	deadline, _ := requestCtx.Deadline()
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	assembler := ipc.NewAssembler(id)
	add := func(f *gen.RecordFragment) error {
		if f == nil || f.InvocationId != invocationID {
			return ipc.ErrMalformed
		}
		return assembler.Add(f)
	}
	if err := add(first); err != nil {
		return status.Error(codes.InvalidArgument, "credential lookup denied")
	}
	for !assembler.Complete() {
		if err := ctx.Err(); err != nil {
			return status.Error(codes.DeadlineExceeded, "credential lookup denied")
		}
		f, err := recvCredentialFragment(ctx, stream)
		if err != nil || add(f) != nil {
			return status.Error(codes.InvalidArgument, "credential lookup denied")
		}
	}
	if _, err := recvCredentialFragment(ctx, stream); !errors.Is(err, io.EOF) {
		return status.Error(codes.InvalidArgument, "credential lookup denied")
	}
	var lookup gen.CredentialLookup
	if assembler.Decode(&lookup) != nil || lookup.Name == "" || lease.invocation.credentials == nil {
		return status.Error(codes.PermissionDenied, "credential lookup denied")
	}
	value, err := lease.invocation.credentials.Get(ctx, lookup.Name)
	if err != nil || !lease.live() || ctx.Err() != nil {
		return status.Error(codes.PermissionDenied, "credential lookup denied")
	}
	if err := ipc.WriteRecord(ipc.Identity{Epoch: id.Epoch, CallID: id.CallID, RecordID: 0, Type: "CredentialValue"}, &gen.CredentialValue{Value: value}, func(f *gen.RecordFragment) error {
		if !lease.live() || ctx.Err() != nil {
			return context.Canceled
		}
		f.InvocationId = invocationID
		return stream.Send(f)
	}); err != nil {
		return status.Error(codes.Internal, "credential lookup denied")
	}
	return nil
}

func recvCredentialFragment(ctx context.Context, stream grpc.BidiStreamingServer[gen.RecordFragment, gen.RecordFragment]) (*gen.RecordFragment, error) {
	type result struct {
		fragment *gen.RecordFragment
		err      error
	}
	ch := make(chan result, 1)
	go func() {
		fragment, err := stream.Recv()
		ch <- result{fragment: fragment, err: err}
	}()
	select {
	case received := <-ch:
		return received.fragment, received.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// CredentialClient exposes one invocation's credentials to a child Connector.
// Its opaque invocation identity is issued by the host, never by request data.
type CredentialClient struct {
	client       gen.HostServicesClient
	epoch        uint64
	invocationID string
}

var _ core.CredentialAccess = (*CredentialClient)(nil)

func NewCredentialClient(client gen.HostServicesClient, epoch uint64, invocationID string) *CredentialClient {
	return &CredentialClient{client: client, epoch: epoch, invocationID: invocationID}
}

func (c *CredentialClient) Get(ctx context.Context, name string) ([]byte, error) {
	return c.getWithCallID(ctx, name, credentialCallID.Add(1))
}

func (c *CredentialClient) getWithCallID(ctx context.Context, name string, callID uint64) ([]byte, error) {
	if c.client == nil || c.invocationID == "" || name == "" {
		return nil, errors.New("credential lookup denied")
	}
	stream, err := c.client.CredentialGet(ctx)
	if err != nil {
		return nil, errors.New("credential lookup denied")
	}
	if err := ipc.WriteRecord(ipc.Identity{Epoch: c.epoch, CallID: callID, RecordID: 0, Type: "CredentialLookup"}, &gen.CredentialLookup{Name: name}, func(f *gen.RecordFragment) error {
		f.InvocationId = c.invocationID
		return stream.Send(f)
	}); err != nil {
		return nil, errors.New("credential lookup denied")
	}
	if err := stream.CloseSend(); err != nil {
		return nil, errors.New("credential lookup denied")
	}
	a := ipc.NewAssembler(ipc.Identity{Epoch: c.epoch, CallID: callID, RecordID: 0, Type: "CredentialValue"})
	for !a.Complete() {
		f, err := stream.Recv()
		if err != nil || f.InvocationId != c.invocationID || a.Add(f) != nil {
			return nil, errors.New("credential lookup denied")
		}
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		return nil, errors.New("credential lookup denied")
	}
	var value gen.CredentialValue
	if err := a.Decode(&value); err != nil {
		return nil, errors.New("credential lookup denied")
	}
	return append([]byte(nil), value.Value...), nil
}

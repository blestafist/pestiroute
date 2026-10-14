//go:build linux

package external

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

const (
	connectorProtocolID = "pestiroute.connector.v1"
	connectorIPCMajor   = 1
)

var (
	ErrHandshakePeer     = errors.New("external connector peer authentication failed")
	ErrHandshakeProtocol = errors.New("external connector IPC protocol mismatch")
	ErrHandshakeVersion  = errors.New("external connector IPC version unsupported")
	ErrHandshakeAPI      = errors.New("external connector API version unsupported")
)

// ConnectConnector authenticates the Unix peer and validates IPC and Connector
// API compatibility before returning a usable connection.
func (p *Process) ConnectConnector(ctx context.Context) (result *grpc.ClientConn, descriptor core.Descriptor, retErr error) {
	defer func() {
		if retErr != nil {
			if result != nil {
				_ = result.Close()
				result = nil
			}
			retErr = p.abort(retErr)
		}
	}()
	ctx, cancel, err := startupContext(ctx, p.StartupDeadline())
	if err != nil {
		return nil, core.Descriptor{}, err
	}
	defer cancel()
	peerConn, err := p.DialConnector(ctx)
	if err != nil {
		return nil, core.Descriptor{}, fmt.Errorf("connect external connector: %w", err)
	}
	var peerMu sync.Mutex
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		peerMu.Lock()
		if peerConn != nil {
			c := peerConn
			peerConn = nil
			peerMu.Unlock()
			return c, nil
		}
		peerMu.Unlock()
		return p.DialConnector(ctx)
	}
	defer func() {
		peerMu.Lock()
		if peerConn != nil {
			_ = peerConn.Close()
		}
		peerMu.Unlock()
	}()
	conn, err := grpc.NewClient("unix://"+p.ConnectorSocket(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(ipc.MaxGRPCMessageBytes),
			grpc.MaxCallSendMsgSize(ipc.MaxGRPCMessageBytes),
		),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return nil, core.Descriptor{}, fmt.Errorf("connect external connector: %w", err)
	}
	result = conn
	client := gen.NewConnectorClient(conn)
	epoch, err := newEpoch()
	if err != nil {
		return nil, core.Descriptor{}, err
	}
	p.epoch = epoch
	var handshake gen.Handshake
	handshakeStream, err := client.Handshake(ctx, bootstrapRequest(epoch))
	if err != nil {
		return nil, core.Descriptor{}, fmt.Errorf("connector handshake: %w", err)
	}
	if err := readRecord(handshakeStream, epoch, "Handshake", &handshake); err != nil {
		return nil, core.Descriptor{}, fmt.Errorf("connector handshake: %w", err)
	}
	if handshake.ProtocolId != connectorProtocolID {
		return nil, core.Descriptor{}, fmt.Errorf("%w: %q", ErrHandshakeProtocol, handshake.ProtocolId)
	}
	if handshake.IpcVersion == nil || handshake.IpcVersion.Major != connectorIPCMajor {
		return nil, core.Descriptor{}, fmt.Errorf("%w: %v", ErrHandshakeVersion, handshake.IpcVersion)
	}
	var wireDescriptor gen.Descriptor
	descriptorStream, err := client.Descriptor(ctx, bootstrapRequest(epoch))
	if err != nil {
		return nil, core.Descriptor{}, fmt.Errorf("connector descriptor: %w", err)
	}
	if err := readRecord(descriptorStream, epoch, "Descriptor", &wireDescriptor); err != nil {
		return nil, core.Descriptor{}, fmt.Errorf("connector descriptor: %w", err)
	}
	descriptor, err = ipc.DescriptorFrom(&wireDescriptor)
	if err != nil || descriptor.Kind != core.ComponentConnector {
		return nil, core.Descriptor{}, fmt.Errorf("%w: malformed Connector descriptor", ipc.ErrMalformed)
	}
	if !descriptor.SupportsAPIVersion(core.APIVersion{Major: 1, Minor: 0}) {
		return nil, core.Descriptor{}, ErrHandshakeAPI
	}
	return result, descriptor.Clone(), nil
}

func bootstrapRequest(epoch uint64) *gen.RecordFragment {
	return &gen.RecordFragment{Epoch: epoch, CallId: 0, RecordId: 0, RecordType: "BootstrapRequest", Final: true}
}

func readRecord(stream grpc.ServerStreamingClient[gen.RecordFragment], epoch uint64, typ string, dst proto.Message) error {
	return readRecordCall(stream, epoch, 0, typ, dst)
}

func readRecordCall(stream grpc.ServerStreamingClient[gen.RecordFragment], epoch, callID uint64, typ string, dst proto.Message) error {
	a := ipc.NewAssembler(ipc.Identity{Epoch: epoch, CallID: callID, RecordID: 0, Type: typ})
	for {
		if a.Complete() {
			if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
				if err == nil {
					return ipc.ErrMalformed
				}
				return err
			}
			return a.Decode(dst)
		}
		f, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return a.Decode(dst)
		}
		if err != nil {
			return err
		}
		if err := a.Add(f); err != nil {
			return err
		}
	}
}

func startupContext(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if deadline.IsZero() || !time.Now().Before(deadline) {
		return nil, nil, errors.New("child startup deadline exceeded")
	}
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	c, cancel := context.WithDeadline(ctx, deadline)
	return c, cancel, nil
}

func newEpoch() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b[:]), nil
}

//go:build linux

package external

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestM6HandshakeRejectsWrongPeer(t *testing.T) {
	assertHandshakeFailure(t, "handshake-wrong-peer", ErrHandshakePeer)
}

func TestM6HandshakeProtocol(t *testing.T) {
	assertHandshakeFailure(t, "handshake-protocol", ErrHandshakeProtocol)
}

func TestM6HandshakeIPCMajor(t *testing.T) {
	assertHandshakeFailure(t, "handshake-ipc-major", ErrHandshakeVersion)
}

func TestM6HandshakeMissingExactAPIPair(t *testing.T) {
	assertHandshakeFailure(t, "handshake-api-version", ErrHandshakeAPI)
}

func TestM6HandshakeMalformedDescriptor(t *testing.T) {
	assertHandshakeFailure(t, "handshake-malformed-descriptor", ipc.ErrMalformed)
}

func TestM6HandshakeOversizedHandshakeFragment(t *testing.T) {
	assertOversizedRecord(t, "handshake-oversized-handshake")
}

func TestM6HandshakeOversizedDescriptorFragment(t *testing.T) {
	assertOversizedRecord(t, "handshake-oversized-descriptor")
}

func TestM6HandshakeTimeoutBeforeInit(t *testing.T) {
	p := startProcess(t, "handshake-stall", startupTimeout)
	p.startupDeadline = time.Now().Add(150 * time.Millisecond)
	started := time.Now()
	conn, _, err := p.ConnectConnector(context.Background())
	if conn != nil {
		t.Fatal("timeout returned a usable connection")
	}
	if status.Code(err) != codes.DeadlineExceeded || time.Since(started) > 5*time.Second {
		t.Fatalf("timeout was not bounded: %v", err)
	}
	assertProcessReaped(t, p)
}

func TestM6HandshakeVersionAxesIndependent(t *testing.T) {
	p := startProcess(t, "handshake-ipc-minor-99", startupTimeout)
	conn, descriptor, err := p.ConnectConnector(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.ID != "fixture" || !descriptor.SupportsAPIVersion(core.APIVersion{Major: 1, Minor: 0}) {
		t.Fatalf("descriptor=%+v", descriptor)
	}
	_ = conn.Close()
	stopTestProcess(t, p)
}

func assertHandshakeFailure(t *testing.T, mode string, want error) {
	t.Helper()
	p := startProcess(t, mode, startupTimeout)
	conn, _, err := p.ConnectConnector(context.Background())
	if conn != nil {
		t.Fatal("rejected handshake returned a usable connection")
	}
	if !errors.Is(err, want) {
		t.Fatalf("ConnectConnector error=%v, want %v", err, want)
	}
	assertProcessReaped(t, p)
}

func assertOversizedRecord(t *testing.T, mode string) {
	t.Helper()
	p := startProcess(t, mode, startupTimeout)
	conn, _, err := p.ConnectConnector(context.Background())
	if conn != nil {
		t.Fatal("oversized record returned a usable connection")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized transport record error=%v, code=%s", err, status.Code(err))
	}
	assertProcessReaped(t, p)
}

func assertProcessReaped(t *testing.T, p *Process) {
	t.Helper()
	select {
	case <-p.waitDone:
	case <-time.After(6 * time.Second):
		t.Fatal("failed startup child was not reaped within supervisor deadline")
	}
	if p.groupExists() {
		t.Fatal("failed startup process group remains")
	}
	if _, err := os.Stat(p.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed startup resources remain: %v", err)
	}
}

func stopTestProcess(t *testing.T, p *Process) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx, func(context.Context) error {
		return syscall.Kill(-p.PID(), syscall.SIGTERM)
	}); err != nil {
		t.Fatal(err)
	}
}

func serveHandshakeChild(socket, mode string) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		os.Exit(31)
	}
	server := grpc.NewServer()
	gen.RegisterConnectorServer(server, &handshakeChild{mode: mode})
	if err := server.Serve(listener); err != nil {
		os.Exit(32)
	}
}

type handshakeChild struct {
	gen.UnimplementedConnectorServer
	mode string
}

func (s *handshakeChild) Handshake(req *gen.RecordFragment, stream grpc.ServerStreamingServer[gen.RecordFragment]) error {
	if s.mode == "handshake-stall" {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	if strings.Contains(s.mode, "oversized-handshake") {
		return stream.Send(&gen.RecordFragment{Epoch: req.Epoch, CallId: req.CallId, RecordId: req.RecordId, RecordType: "Handshake", TotalLength: 1, Final: true, Data: make([]byte, ipc.MaxGRPCMessageBytes+1)})
	}
	msg := &gen.Handshake{ProtocolId: connectorProtocolID, IpcVersion: &gen.Version{Major: 1}}
	switch s.mode {
	case "handshake-protocol":
		msg.ProtocolId = "other"
	case "handshake-ipc-major":
		msg.IpcVersion.Major = 2
	case "handshake-ipc-minor-99":
		msg.IpcVersion.Minor = 99
	}
	return ipc.WriteRecord(ipc.Identity{Epoch: req.Epoch, CallID: req.CallId, RecordID: req.RecordId, Type: "Handshake"}, msg, stream.Send)
}

func (s *handshakeChild) Descriptor(req *gen.RecordFragment, stream grpc.ServerStreamingServer[gen.RecordFragment]) error {
	if strings.Contains(s.mode, "oversized-descriptor") {
		return stream.Send(&gen.RecordFragment{Epoch: req.Epoch, CallId: req.CallId, RecordId: req.RecordId, RecordType: "Descriptor", TotalLength: 1, Final: true, Data: make([]byte, ipc.MaxGRPCMessageBytes+1)})
	}
	d := &gen.Descriptor{Id: "fixture", Kind: "connector", ImplementationVersion: "fixture-2", ApiVersions: []*gen.Version{{Major: 1, Minor: 0}}, Protocols: []string{"test.v1"}, ConnectorType: "test"}
	switch s.mode {
	case "handshake-api-version":
		d.ApiVersions = []*gen.Version{{Major: 1, Minor: 1}}
	case "handshake-malformed-descriptor":
		d = &gen.Descriptor{}
	}
	return ipc.WriteRecord(ipc.Identity{Epoch: req.Epoch, CallID: req.CallId, RecordID: req.RecordId, Type: "Descriptor"}, d, stream.Send)
}

var _ gen.ConnectorServer = (*handshakeChild)(nil)

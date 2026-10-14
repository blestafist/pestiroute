//go:build linux

package external

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	"google.golang.org/protobuf/proto"
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

func (s *handshakeChild) Init(stream grpc.ClientStreamingServer[gen.RecordFragment, gen.RecordFragment]) error {
	if s.mode == "lifecycle-init-stall" {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	var a *ipc.Assembler
	for {
		f, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if a == nil {
			a = ipc.NewAssembler(ipc.Identity{Epoch: f.Epoch, CallID: 1, RecordID: 0, Type: "ComponentConfig"})
		}
		if err := a.Add(f); err != nil {
			return status.Error(codes.InvalidArgument, "malformed")
		}
	}
	var config gen.ComponentConfig
	if err := a.Decode(&config); err != nil {
		return status.Error(codes.InvalidArgument, "malformed")
	}
	if s.mode == "lifecycle-init-fail" {
		return status.Error(codes.Internal, "private child error")
	}
	return stream.SendAndClose(&gen.RecordFragment{})
}

func (s *handshakeChild) Health(req *gen.RecordFragment, stream grpc.ServerStreamingServer[gen.RecordFragment]) error {
	if s.mode == "lifecycle-health-stall" {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	state := core.HealthReady
	if s.mode == "lifecycle-health-unavailable" {
		state = core.HealthUnavailable
	}
	if s.mode == "lifecycle-health-unknown" {
		state = core.HealthUnknown
	}
	diagnostic := "fixture health"
	if s.mode == "lifecycle-health-malicious" {
		diagnostic = "token=private-health-secret\nunsafe child detail"
	}
	if s.mode == "lifecycle-health-oversized" {
		diagnostic = strings.Repeat("private-health-secret", 8<<10)
	}
	return ipc.WriteRecord(ipc.Identity{Epoch: req.Epoch, CallID: 1, RecordID: 0, Type: "Health"}, ipc.HealthTo(core.Health{State: state, Diagnostic: diagnostic}), stream.Send)
}

func (s *handshakeChild) Capabilities(stream grpc.BidiStreamingServer[gen.RecordFragment, gen.RecordFragment]) error {
	_, _ = fmt.Fprintln(os.Stderr, "lifecycle-capabilities-called")
	var a *ipc.Assembler
	var epoch uint64
	for {
		f, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if a == nil {
			epoch = f.Epoch
			a = ipc.NewAssembler(ipc.Identity{Epoch: f.Epoch, CallID: 1, RecordID: 0, Type: "CapabilityScope"})
		}
		if err := a.Add(f); err != nil {
			return status.Error(codes.InvalidArgument, "malformed")
		}
	}
	var scope gen.CapabilityScope
	if err := a.Decode(&scope); err != nil || scope.Protocol != "test.v1" || scope.Mode != "native" {
		return status.Error(codes.InvalidArgument, "invalid scope")
	}
	result := &gen.CapabilityResult{Values: []*gen.CapabilityValue{{Key: "llm.streaming", State: "supported"}, {Key: "llm.tools", State: "unknown"}}}
	if s.mode == "lifecycle-capabilities-large" {
		if len(scope.Model) < ipc.MaxFragmentBytes {
			return status.Error(codes.InvalidArgument, "scope did not cross fragment boundary")
		}
		result.Values = make([]*gen.CapabilityValue, 5000)
		for i := range result.Values {
			result.Values[i] = &gen.CapabilityValue{Key: fmt.Sprintf("test.capability.%04d", i), State: "supported"}
		}
	}
	return ipc.WriteRecord(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: 0, Type: "CapabilityResult"}, result, stream.Send)
}

func (s *handshakeChild) Models(stream grpc.BidiStreamingServer[gen.RecordFragment, gen.RecordFragment]) error {
	_, _ = fmt.Fprintln(os.Stderr, "lifecycle-models-called")
	if s.mode == "lifecycle-models-stall" {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	var query gen.ModelQuery
	epoch, err := receiveBidiRecord(stream, "ModelQuery", &query)
	if err != nil {
		return err
	}
	if query.Protocol != "test.v1" || query.Mode != core.ModeTranslation || query.AccountId != "account" {
		return status.Error(codes.InvalidArgument, "invalid model scope")
	}
	available := false
	result := &gen.ModelsResult{Supported: true, Models: []*gen.ModelInfo{
		{Id: strings.Repeat("m", 90<<10), Capabilities: []*gen.CapabilityValue{{Key: "llm.streaming", State: "supported"}, {Key: "llm.tools", State: "unknown"}}, Available: &available},
		{Id: "availability-unknown", Capabilities: []*gen.CapabilityValue{{Key: "llm.audio", State: "unsupported"}}},
	}}
	return ipc.WriteRecord(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: 0, Type: "ModelsResult"}, result, stream.Send)
}

func (s *handshakeChild) EstimateUsage(stream grpc.BidiStreamingServer[gen.EstimateFrame, gen.RecordFragment]) error {
	_, _ = fmt.Fprintln(os.Stderr, "lifecycle-estimate-called")
	if s.mode == "lifecycle-estimate-stall" {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	var query gen.UsageQuery
	var epoch uint64
	assembler := ipc.NewAssembler(ipc.Identity{CallID: 1, RecordID: 0, Type: "UsageQuery"})
	var body *ipc.BodyAssembler
	bodyStarted := false
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if frame.CallId != 1 || (epoch != 0 && frame.Epoch != epoch) {
			return ipc.ErrMalformed
		}
		if epoch == 0 {
			epoch = frame.Epoch
			assembler = ipc.NewAssembler(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: 0, Type: "UsageQuery"})
		}
		switch payload := frame.Payload.(type) {
		case *gen.EstimateFrame_Record:
			if bodyStarted {
				return ipc.ErrMalformed
			}
			if err := assembler.Add(payload.Record); err != nil {
				return err
			}
		case *gen.EstimateFrame_Body:
			if !assembler.Complete() {
				return ipc.ErrMalformed
			}
			if body == nil {
				if err := assembler.Decode(&query); err != nil || query.MaxBodyBytes < 0 {
					return ipc.ErrMalformed
				}
				body = ipc.NewBodyAssembler(uint64(query.MaxBodyBytes))
			}
			bodyStarted = true
			chunk, err := ipc.BodyChunkFrom(payload.Body)
			if err != nil {
				return err
			}
			if err := body.Add(chunk); err != nil {
				return err
			}
		default:
			return ipc.ErrMalformed
		}
	}
	if !assembler.Complete() {
		return ipc.ErrMalformed
	}
	if body == nil {
		if err := assembler.Decode(&query); err != nil || query.MaxBodyBytes < 0 {
			return ipc.ErrMalformed
		}
		body = ipc.NewBodyAssembler(uint64(query.MaxBodyBytes))
	}
	if query.Protocol != "test.v1" || query.Mode != core.ModeNative || query.Model != "fixture" || query.AccountId != "account" || query.MaxBodyBytes != int64(ipc.MaxFragmentBytes+19) || query.Payload == nil || query.Payload.Protocol != "test.v1" || query.Payload.ContentType != "application/octet-stream" {
		return status.Error(codes.InvalidArgument, "invalid estimate scope")
	}
	bodyBytes, err := body.Finish()
	if err != nil {
		return err
	}
	if len(bodyBytes) != ipc.MaxFragmentBytes+19 || !bytes.Equal(bodyBytes, bytes.Repeat([]byte{0xab}, len(bodyBytes))) {
		return ipc.ErrMalformed
	}
	zero := int64(0)
	usage := &gen.UsageReport{InputTokens: &zero, Source: string(core.UsageEstimate), Completeness: string(core.UsagePartial)}
	return ipc.WriteRecord(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: 0, Type: "EstimateResult"}, &gen.EstimateResult{Supported: true, Known: false, Usage: usage, Method: strings.Repeat("estimate", 12<<10)}, stream.Send)
}

func receiveBidiRecord(stream grpc.BidiStreamingServer[gen.RecordFragment, gen.RecordFragment], typ string, dst proto.Message) (uint64, error) {
	f, err := stream.Recv()
	if err != nil {
		return 0, err
	}
	epoch := f.Epoch
	a := ipc.NewAssembler(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: 0, Type: typ})
	for {
		if err := a.Add(f); err != nil {
			return 0, err
		}
		if a.Complete() {
			break
		}
		f, err = stream.Recv()
		if err != nil {
			return 0, err
		}
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		if err == nil {
			return 0, ipc.ErrMalformed
		}
		return 0, err
	}
	return epoch, a.Decode(dst)
}

func (s *handshakeChild) Close(context.Context, *gen.RecordFragment) (*gen.RecordFragment, error) {
	_, _ = fmt.Fprintln(os.Stderr, "lifecycle-close-called")
	if s.mode == "lifecycle-close-delay" {
		time.Sleep(350 * time.Millisecond)
	}
	if s.mode == "lifecycle-close-fail" {
		return nil, status.Error(codes.Internal, "private child error")
	}
	return &gen.RecordFragment{}, nil
}

var _ gen.ConnectorServer = (*handshakeChild)(nil)

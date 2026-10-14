//go:build linux

package external

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
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

func TestM6HandshakeRequiresFlowControlMinor(t *testing.T) {
	assertHandshakeFailure(t, "handshake-ipc-minor-0", ErrHandshakeVersion)
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

func serveHandshakeChild(socket, mode, marker, attemptMarker string) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		os.Exit(31)
	}
	server := grpc.NewServer()
	gen.RegisterConnectorServer(server, &handshakeChild{mode: mode, marker: marker, attemptMarker: attemptMarker})
	if err := server.Serve(listener); err != nil {
		os.Exit(32)
	}
}

type handshakeChild struct {
	gen.UnimplementedConnectorServer
	mode          string
	marker        string
	attemptMarker string
	flowMu        sync.Mutex
	flow          *ChildFlowControl
	flowConn      *grpc.ClientConn
}

func (s *handshakeChild) ensureFlowControl(epoch uint64) (*ChildFlowControl, error) {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	if s.flow != nil {
		if s.flow.epoch != epoch {
			return nil, errCreditIdentity
		}
		return s.flow, nil
	}
	flow, conn, err := DialChildFlowControl(context.Background(), os.Getenv(hostServicesSocketEnv), epoch)
	if err != nil {
		return nil, err
	}
	s.flow, s.flowConn = flow, conn
	return flow, nil
}

func (s *handshakeChild) FlowControl(ctx context.Context, req *gen.FlowControlRequest) (*gen.FlowControlResponse, error) {
	if req == nil || req.Identity == nil || req.Identity.Epoch == 0 || proto.Size(req) > maxControlMessageBytes {
		return nil, status.Error(codes.InvalidArgument, "invalid flow control message")
	}
	flow, err := s.ensureFlowControl(req.Identity.Epoch)
	if err != nil {
		return nil, err
	}
	return flow.FlowControl(ctx, req)
}

func (s *handshakeChild) Handshake(req *gen.RecordFragment, stream grpc.ServerStreamingServer[gen.RecordFragment]) error {
	if s.mode == "handshake-stall" {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	if strings.Contains(s.mode, "oversized-handshake") {
		return stream.Send(&gen.RecordFragment{Epoch: req.Epoch, CallId: req.CallId, RecordId: req.RecordId, RecordType: "Handshake", TotalLength: 1, Final: true, Data: make([]byte, ipc.MaxGRPCMessageBytes+1)})
	}
	msg := &gen.Handshake{ProtocolId: connectorProtocolID, IpcVersion: &gen.Version{Major: 1, Minor: connectorIPCMinor}}
	switch s.mode {
	case "handshake-protocol":
		msg.ProtocolId = "other"
	case "handshake-ipc-major":
		msg.IpcVersion.Major = 2
	case "handshake-ipc-minor-99":
		msg.IpcVersion.Minor = 99
	case "handshake-ipc-minor-0":
		msg.IpcVersion.Minor = 0
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
	err := stream.SendAndClose(&gen.RecordFragment{})
	if err == nil && (s.mode == "supervisor-crash-loop" || (strings.HasPrefix(s.mode, "supervisor-crash-once") && s.marker != "" && createMarker(s.marker))) {
		go func() { time.Sleep(200 * time.Millisecond); _ = syscall.Kill(os.Getpid(), syscall.SIGKILL) }()
	}
	return err
}

func createMarker(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
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

func (s *handshakeChild) Authenticate(stream grpc.BidiStreamingServer[gen.RecordFragment, gen.RecordFragment]) error {
	_, _ = fmt.Fprintln(os.Stderr, "lifecycle-authenticate-called")
	var req gen.AuthRequest
	epoch, invocationID, err := receiveAuthRequest(stream, &req)
	if err != nil {
		return err
	}
	if req.AccountId != "account" || (req.Action != "start" && req.Action != "continue" && req.Action != "refresh") {
		return status.Error(codes.InvalidArgument, "invalid auth request")
	}
	if s.mode == "auth-continue" && (len(req.State) != (90<<10)+2 || !bytes.Equal(req.State[:2], []byte{0, 0xff})) {
		return status.Error(codes.InvalidArgument, "invalid opaque continuation")
	}
	if s.mode == "auth-stall" {
		<-stream.Context().Done()
		if _, err := NewCredentialClient(childHostServices(), epoch, invocationID).Get(context.Background(), "token"); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "auth-cancel-callback-denied")
		} else {
			_, _ = fmt.Fprintln(os.Stderr, "auth-cancel-callback-granted")
		}
		return stream.Context().Err()
	}
	result := &gen.AuthResult{Supported: true}
	switch s.mode {
	case "auth-start":
		result.State = append([]byte{0, 0xff, 's'}, make([]byte, 70<<10)...)
		result.NextAction = "continue"
		result.UserAction = &gen.AuthUserAction{VerificationUri: "https://example.test/device", UserCode: "ABC-123", PollIntervalNs: int64(5 * time.Second)}
	case "auth-continue":
		result.Credentials = map[string][]byte{"access": append([]byte{0, 0xff}, make([]byte, 70<<10)...)}
		expires := time.Date(2030, 2, 3, 4, 5, 6, 7, time.FixedZone("fixture", 3600))
		result.CredentialExpiresAt = timestamppb.New(expires)
	case "auth-refresh":
		value, err := NewCredentialClient(childHostServices(), epoch, invocationID).Get(stream.Context(), "token")
		if err != nil {
			return status.Error(codes.PermissionDenied, "credential lookup denied")
		}
		result.Credentials = map[string][]byte{"refresh": value}
		expires := time.Date(2031, 3, 4, 5, 6, 7, 0, time.UTC)
		result.CredentialExpiresAt = timestamppb.New(expires)
	case "auth-unsupported":
		result.Supported = false
	case "auth-malformed":
		result.CredentialExpiresAt = &timestamppb.Timestamp{Seconds: 1 << 40}
	default:
		return status.Error(codes.InvalidArgument, "unknown auth fixture")
	}
	err = ipc.WriteRecord(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: 0, Type: "AuthResult"}, result, func(fragment *gen.RecordFragment) error {
		fragment.InvocationId = invocationID
		return stream.Send(fragment)
	})
	if err == nil && s.mode == "auth-refresh" {
		go func() {
			time.Sleep(100 * time.Millisecond)
			if _, err := NewCredentialClient(childHostServices(), epoch, invocationID).Get(context.Background(), "token"); err != nil {
				_, _ = fmt.Fprintln(os.Stderr, "auth-complete-callback-denied")
			} else {
				_, _ = fmt.Fprintln(os.Stderr, "auth-complete-callback-granted")
			}
		}()
	}
	return err
}

func (s *handshakeChild) Execute(stream grpc.BidiStreamingServer[gen.ExecuteFrame, gen.ExecuteFrame]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first == nil || first.Epoch == 0 || first.CallId != 1 || first.InvocationId == "" {
		return status.Error(codes.InvalidArgument, "invalid execute identity")
	}
	epoch, invocationID := first.Epoch, first.InvocationId
	flow, err := s.ensureFlowControl(epoch)
	if err != nil {
		return err
	}
	invocationCtx, cancelInvocation := context.WithCancel(stream.Context())
	if err := flow.RegisterInvocation(invocationID, cancelInvocation); err != nil {
		cancelInvocation()
		return status.Error(codes.InvalidArgument, "duplicate execute invocation")
	}
	defer func() { flow.UnregisterInvocation(invocationID); cancelInvocation() }()
	r, ok := first.Payload.(*gen.ExecuteFrame_Record)
	if !ok || r.Record == nil || r.Record.RecordType != "ExecuteStart" || r.Record.InvocationId != invocationID {
		return status.Error(codes.InvalidArgument, "missing execute start")
	}
	a := ipc.NewAssembler(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: 0, Type: "ExecuteStart"})
	if err := validateChildExecuteFrame(invocationCtx, flow, first, len(r.Record.Data)); err != nil {
		return status.Error(codes.InvalidArgument, "unreserved execute start")
	}
	if err := a.Add(r.Record); err != nil {
		return status.Error(codes.InvalidArgument, "malformed execute start")
	}
	for !a.Complete() {
		f, err := stream.Recv()
		if err != nil {
			return err
		}
		if !sameExecuteIdentity(f, epoch, invocationID) {
			return status.Error(codes.InvalidArgument, "execute identity mismatch")
		}
		r, ok := f.Payload.(*gen.ExecuteFrame_Record)
		if !ok || r.Record == nil || r.Record.InvocationId != invocationID {
			return status.Error(codes.InvalidArgument, "invalid execute start fragment")
		}
		if err := validateChildExecuteFrame(invocationCtx, flow, f, len(r.Record.Data)); err != nil {
			return status.Error(codes.InvalidArgument, "unreserved execute start fragment")
		}
		if err := a.Add(r.Record); err != nil {
			return status.Error(codes.InvalidArgument, "malformed execute start")
		}
	}
	var start gen.ExecuteStart
	if err := a.Decode(&start); err != nil || start.Request == nil || start.Scope == nil {
		return status.Error(codes.InvalidArgument, "invalid execute start")
	}
	if start.MaxBodyBytes <= 0 {
		return status.Error(codes.InvalidArgument, "invalid execute body limit")
	}
	var body []byte
	for {
		f, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "missing input complete")
		}
		if err != nil {
			return err
		}
		if !sameExecuteIdentity(f, epoch, invocationID) {
			return status.Error(codes.InvalidArgument, "execute identity mismatch")
		}
		switch p := f.Payload.(type) {
		case *gen.ExecuteFrame_Body:
			chunk, err := ipc.BodyChunkFrom(p.Body)
			if err != nil {
				return status.Error(codes.InvalidArgument, "invalid body chunk")
			}
			if uint64(len(chunk)) > uint64(start.MaxBodyBytes)-uint64(len(body)) {
				return status.Error(codes.InvalidArgument, "execute body exceeds admitted route limit")
			}
			if err := validateChildExecuteFrame(invocationCtx, flow, f, len(chunk)); err != nil {
				return status.Error(codes.InvalidArgument, "unreserved execute body")
			}
			body = append(body, chunk...)
		case *gen.ExecuteFrame_Record:
			if p.Record == nil || p.Record.InvocationId != invocationID || p.Record.RecordType != "InputComplete" || p.Record.RecordId != 1 {
				return status.Error(codes.InvalidArgument, "invalid input terminator")
			}
			var end gen.InputComplete
			if err := validateChildExecuteFrame(invocationCtx, flow, f, len(p.Record.Data)); err != nil {
				return status.Error(codes.InvalidArgument, "unreserved input terminator")
			}
			if err := ipc.Reassemble(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: 1, Type: "InputComplete"}, []*gen.RecordFragment{p.Record}, &end); err != nil {
				return status.Error(codes.InvalidArgument, "malformed input terminator")
			}
			goto inputDone
		default:
			return status.Error(codes.InvalidArgument, "unexpected execute frame")
		}
	}
inputDone:
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		return status.Error(codes.InvalidArgument, "unexpected data after input complete")
	}
	if s.mode == "execute-stall" {
		_, _ = fmt.Fprintln(os.Stderr, "execute-stall-called")
		<-stream.Context().Done()
		_, _ = fmt.Fprintln(os.Stderr, "execute-stall-cancel-ack")
		return stream.Context().Err()
	}
	if strings.HasSuffix(s.mode, "-execute") {
		if s.attemptMarker != "" {
			f, err := os.OpenFile(s.attemptMarker, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
			if err == nil {
				_, _ = fmt.Fprintln(f, "attempt")
				_ = f.Close()
			}
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	if s.mode == "execute-child-cancelled-before-head" {
		_, _ = fmt.Fprintln(os.Stderr, "execute-child-cancelled-before-head")
		return status.Error(codes.Canceled, "child cancelled execution")
	}
	if s.mode == "execute-child-deadline-before-head" {
		_, _ = fmt.Fprintln(os.Stderr, "execute-child-deadline-before-head")
		return status.Error(codes.DeadlineExceeded, "child deadline expired")
	}
	if s.mode == "execute-error" {
		ge := &core.GatewayError{Code: "rejected", Category: core.CategoryRateLimited, Retryable: true, RetryDisposition: core.RetrySafe, Provider: "fixture", OriginalError: "diagnostic", Message: "try later"}
		if err := sendExecuteRecord(stream, flow, invocationCtx, epoch, invocationID, 0, "GatewayError", ipc.GatewayErrorTo(ge)); err != nil {
			return err
		}
		return flow.WaitDrained(invocationCtx, invocationID)
	}
	if s.mode == "supervisor-valid-invalid-request" {
		ge := &core.GatewayError{Code: "invalid_request", Category: core.CategoryInvalidRequest, RetryDisposition: core.RetryUnsafe, Message: "fixture rejected this request"}
		if err := sendExecuteRecord(stream, flow, invocationCtx, epoch, invocationID, 0, "GatewayError", ipc.GatewayErrorTo(ge)); err != nil {
			return err
		}
		return flow.WaitDrained(invocationCtx, invocationID)
	}
	if s.mode == "execute-crash-before-head" {
		os.Exit(23)
	}
	if s.mode == "execute-bad-epoch" {
		return stream.Send(&gen.ExecuteFrame{Epoch: epoch + 1, CallId: 1, InvocationId: invocationID, Payload: &gen.ExecuteFrame_Record{Record: &gen.RecordFragment{Epoch: epoch + 1, CallId: 1, RecordId: 0, RecordType: "HeadFrame", TotalLength: 0, Final: true, InvocationId: invocationID}}})
	}
	statusCode := int32(207)
	head := &gen.HeadFrame{Protocol: start.Request.Payload.Protocol, ContentType: "application/octet-stream", HttpStatus: &statusCode, Headers: map[string]*gen.StringList{"X-Fixture": {Values: []string{"fixed"}}}}
	if s.mode == "execute-incremental-before-head" {
		chunk, err := ipc.BodyChunkTo([]byte("early"))
		if err != nil {
			return err
		}
		return sendExecuteData(stream, flow, invocationCtx, epoch, invocationID, len(chunk.Data), &gen.ExecuteFrame{Payload: &gen.ExecuteFrame_Body{Body: chunk}})
	}
	if err := sendExecuteRecord(stream, flow, invocationCtx, epoch, invocationID, 0, "HeadFrame", head); err != nil {
		return err
	}
	if s.mode == "execute-child-cancelled-after-head" {
		_, _ = fmt.Fprintln(os.Stderr, "execute-child-cancelled-after-head")
		time.Sleep(100 * time.Millisecond)
		return status.Error(codes.Canceled, "child cancelled after Head")
	}
	if s.mode == "execute-child-deadline-after-head" {
		_, _ = fmt.Fprintln(os.Stderr, "execute-child-deadline-after-head")
		time.Sleep(100 * time.Millisecond)
		return status.Error(codes.DeadlineExceeded, "child deadline after Head")
	}
	if s.mode == "execute-crash-after-head" {
		time.Sleep(100 * time.Millisecond)
		os.Exit(23)
	}
	if s.mode == "execute-eof-after-head" {
		time.Sleep(100 * time.Millisecond)
		return nil
	}
	if s.mode == "execute-bad-sequence" {
		time.Sleep(100 * time.Millisecond)
		chunk, err := ipc.BodyChunkTo([]byte("bad sequence"))
		if err != nil {
			return err
		}
		return stream.Send(&gen.ExecuteFrame{Epoch: epoch, CallId: 1, InvocationId: invocationID, DataSequence: 99, Payload: &gen.ExecuteFrame_Body{Body: chunk}})
	}
	if s.mode == "execute-callback-credential" {
		if _, err := NewCredentialClient(childHostServices(), epoch, invocationID).Get(stream.Context(), "token"); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "execute-credential-cancel-ack")
		} else {
			_, _ = fmt.Fprintln(os.Stderr, "execute-credential-unexpected-success")
		}
		if _, err := NewCredentialClient(childHostServices(), epoch, invocationID).Get(context.Background(), "token"); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "execute-late-callback-denied")
		} else {
			_, _ = fmt.Fprintln(os.Stderr, "execute-late-callback-authorized")
		}
		return stream.Context().Err()
	}
	if s.mode == "execute-callback-http" {
		client := NewHTTPDoerClient(childHostServices(), flow, epoch, invocationID)
		request, _ := http.NewRequestWithContext(stream.Context(), http.MethodGet, "http://example.invalid/", nil)
		if _, err := client.Do(request); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "execute-http-cancel-ack")
		} else {
			_, _ = fmt.Fprintln(os.Stderr, "execute-http-unexpected-success")
		}
		request, _ = http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
		if _, err := client.Do(request); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "execute-late-callback-denied")
		} else {
			_, _ = fmt.Fprintln(os.Stderr, "execute-late-callback-authorized")
		}
		return stream.Context().Err()
	}
	if s.mode == "execute-head-stall" {
		_, _ = fmt.Fprintln(os.Stderr, "execute-posthead-waiting")
		if _, err := NewCredentialClient(childHostServices(), epoch, invocationID).Get(stream.Context(), "token"); err != nil {
			return status.Error(codes.PermissionDenied, "execute callback denied before cancellation")
		}
		_, _ = fmt.Fprintln(os.Stderr, "execute-posthead-authorized")
		<-stream.Context().Done()
		if _, err := NewCredentialClient(childHostServices(), epoch, invocationID).Get(context.Background(), "token"); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "execute-posthead-revoked")
		} else {
			_, _ = fmt.Fprintln(os.Stderr, "execute-posthead-still-authorized")
		}
		return stream.Context().Err()
	}
	if len(body) > 0 {
		writeBody := func(chunk []byte) error {
			b, err := ipc.BodyChunkTo(chunk)
			if err != nil {
				return err
			}
			return sendExecuteData(stream, flow, invocationCtx, epoch, invocationID, len(chunk), &gen.ExecuteFrame{Payload: &gen.ExecuteFrame_Body{Body: b}})
		}
		if s.mode == "execute-incremental" {
			if len(body) <= ipc.MaxFragmentBytes {
				return status.Error(codes.InvalidArgument, "incremental fixture requires multiple chunks")
			}
			if err := writeBody(body[:ipc.MaxFragmentBytes]); err != nil {
				return err
			}
			if _, err := NewCredentialClient(childHostServices(), epoch, invocationID).Get(stream.Context(), "token"); err != nil {
				return status.Error(codes.PermissionDenied, "incremental gate denied")
			}
			if err := ipc.WriteBody(body[ipc.MaxFragmentBytes:], uint64(len(body)-ipc.MaxFragmentBytes), writeBody); err != nil {
				return err
			}
		} else if err := ipc.WriteBody(body, uint64(len(body)), writeBody); err != nil {
			return err
		}
	}
	inputTokens := int64(12)
	complete := &gen.CompleteFrame{Outcome: string(core.OutcomeSucceeded), Usage: &gen.UsageReport{InputTokens: &inputTokens, Source: string(core.UsageProvider), Completeness: string(core.UsageComplete)}}
	switch s.mode {
	case "execute-incremental-failed":
		complete.Outcome = string(core.OutcomeFailed)
		complete.Error = ipc.GatewayErrorTo(&core.GatewayError{Code: "fixture_failed", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnsafe, Message: "fixture failed"})
	case "execute-incremental-cancelled":
		complete.Outcome = string(core.OutcomeCancelled)
		complete.Error = ipc.GatewayErrorTo(&core.GatewayError{Code: "fixture_cancelled", Category: core.CategoryCancelled, RetryDisposition: core.RetryUnsafe, Message: "fixture cancelled"})
	case "execute-incremental-incomplete":
		complete.Outcome = string(core.OutcomeIncomplete)
		complete.Error = ipc.GatewayErrorTo(&core.GatewayError{Code: "fixture_incomplete", Category: core.CategoryInternal, RetryDisposition: core.RetryUnknown, Message: "fixture incomplete"})
	}
	if err := sendExecuteRecord(stream, flow, invocationCtx, epoch, invocationID, 2, "CompleteFrame", complete); err != nil {
		return err
	}
	if s.mode == "execute-incremental-after-complete" {
		chunk, err := ipc.BodyChunkTo([]byte("late"))
		if err != nil {
			return err
		}
		return stream.Send(&gen.ExecuteFrame{Epoch: epoch, CallId: 1, InvocationId: invocationID, Payload: &gen.ExecuteFrame_Body{Body: chunk}})
	}
	if s.mode == "execute-incremental-duplicate-complete" {
		return sendExecuteRecord(stream, flow, invocationCtx, epoch, invocationID, 2, "CompleteFrame", complete)
	}
	return flow.WaitDrained(invocationCtx, invocationID)
}

func sameExecuteIdentity(f *gen.ExecuteFrame, epoch uint64, invocationID string) bool {
	return f != nil && f.Epoch == epoch && f.CallId == 1 && f.InvocationId == invocationID
}

func validateChildExecuteFrame(ctx context.Context, flow *ChildFlowControl, frame *gen.ExecuteFrame, appBytes int) error {
	id := creditIdentity{epoch: frame.Epoch, invocationID: frame.InvocationId, operation: "Execute", callID: frame.CallId, direction: "host_to_child", sequence: frame.DataSequence}
	if err := flow.ValidateIncoming(id, appBytes); err != nil {
		return err
	}
	return flow.AckIncoming(ctx, id)
}

func sendExecuteData(stream grpc.BidiStreamingServer[gen.ExecuteFrame, gen.ExecuteFrame], flow *ChildFlowControl, ctx context.Context, epoch uint64, invocationID string, appBytes int, frame *gen.ExecuteFrame) error {
	id, err := flow.ReserveNext(ctx, invocationID, "Execute", 1, appBytes)
	if err != nil {
		return err
	}
	frame.Epoch, frame.CallId, frame.InvocationId, frame.DataSequence = epoch, 1, invocationID, id.sequence
	return stream.Send(frame)
}

func sendExecuteRecord(stream grpc.BidiStreamingServer[gen.ExecuteFrame, gen.ExecuteFrame], flow *ChildFlowControl, ctx context.Context, epoch uint64, invocationID string, recordID uint64, typ string, value proto.Message) error {
	return ipc.WriteRecord(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: recordID, Type: typ}, value, func(f *gen.RecordFragment) error {
		f.InvocationId = invocationID
		return sendExecuteData(stream, flow, ctx, epoch, invocationID, len(f.Data), &gen.ExecuteFrame{Payload: &gen.ExecuteFrame_Record{Record: f}})
	})
}

func childHostServices() gen.HostServicesClient {
	socket := os.Getenv(hostServicesSocketEnv)
	conn, err := grpc.NewClient("passthrough:///child-host-services", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		panic("host services connection failed")
	}
	return gen.NewHostServicesClient(conn)
}

func receiveAuthRequest(stream grpc.BidiStreamingServer[gen.RecordFragment, gen.RecordFragment], dst proto.Message) (uint64, string, error) {
	first, err := stream.Recv()
	if err != nil || first == nil || first.InvocationId == "" {
		return 0, "", ipc.ErrMalformed
	}
	epoch, invocationID := first.Epoch, first.InvocationId
	a := ipc.NewAssembler(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: 0, Type: "AuthRequest"})
	for {
		if first.InvocationId != invocationID || a.Add(first) != nil {
			return 0, "", ipc.ErrMalformed
		}
		if a.Complete() {
			break
		}
		first, err = stream.Recv()
		if err != nil || first == nil {
			return 0, "", ipc.ErrMalformed
		}
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		return 0, "", ipc.ErrMalformed
	}
	if err := a.Decode(dst); err != nil {
		return 0, "", err
	}
	return epoch, invocationID, nil
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

//go:build linux

package external

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/blestafist/pestiroute/internal/connector/responses"
	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ConnectorChild adapts a Core Connector to M6 RPC; runtime services remain
// invocation-scoped reverse calls.
type ConnectorChild struct {
	gen.UnimplementedConnectorServer
	connector  core.Connector
	flow       *ChildFlowControl
	host       gen.HostServicesClient
	epoch      uint64
	mu         sync.Mutex
	closed     bool
	stop       func()
	logMu      sync.Mutex
	logParent  context.Context
	logCancel  context.CancelFunc
	logWorker  *LogDispatcher
	logStopped bool
}

var _ gen.ConnectorServer = (*ConnectorChild)(nil)

func NewConnectorChild(connector core.Connector, host gen.HostServicesClient, flow *ChildFlowControl, epoch uint64) (*ConnectorChild, error) {
	if connector == nil || host == nil || flow == nil || epoch == 0 {
		return nil, errors.New("invalid child connector services")
	}
	return &ConnectorChild{connector: connector, host: host, flow: flow, epoch: epoch}, nil
}

func NewResponsesChild(host gen.HostServicesClient, flow *ChildFlowControl, epoch uint64) (*ConnectorChild, error) {
	return NewConnectorChild(responses.NewConnector(), host, flow, epoch)
}

func (s *ConnectorChild) Handshake(req *gen.RecordFragment, stream grpc.ServerStreamingServer[gen.RecordFragment]) error {
	if req == nil || req.Epoch == 0 || req.CallId != 0 || req.RecordId != 0 || req.RecordType != "BootstrapRequest" {
		return ipc.ErrMalformed
	}
	if err := s.flow.SetProcessEpoch(req.Epoch); err != nil {
		return err
	}
	s.resetLogEpoch(req.Epoch)
	return ipc.WriteRecord(ipc.Identity{Epoch: s.epoch, CallID: req.CallId, RecordID: 0, Type: "Handshake"}, &gen.Handshake{ProtocolId: "pestiroute.connector.v1", IpcVersion: &gen.Version{Major: 1, Minor: 1}}, stream.Send)
}

func (s *ConnectorChild) Descriptor(req *gen.RecordFragment, stream grpc.ServerStreamingServer[gen.RecordFragment]) error {
	if req == nil || req.Epoch != s.epoch {
		return ipc.ErrMalformed
	}
	return ipc.WriteRecord(ipc.Identity{Epoch: s.epoch, CallID: req.CallId, RecordID: 0, Type: "Descriptor"}, ipc.DescriptorTo(s.connector.Descriptor()), stream.Send)
}

func (s *ConnectorChild) Init(stream grpc.ClientStreamingServer[gen.RecordFragment, gen.RecordFragment]) error {
	cfg := new(gen.ComponentConfig)
	if err := receiveRecord(stream, s.epoch, 1, 0, "ComponentConfig", cfg); err != nil {
		return err
	}
	v, err := ipc.ConfigFrom(cfg)
	if err == nil {
		err = s.connector.Init(stream.Context(), v)
	}
	if err != nil {
		return status.Error(3, "connector initialization failed")
	}
	return stream.SendAndClose(&gen.RecordFragment{Epoch: s.epoch, CallId: 1})
}

func (s *ConnectorChild) Health(req *gen.RecordFragment, stream grpc.ServerStreamingServer[gen.RecordFragment]) error {
	if req == nil || req.Epoch != s.epoch {
		return ipc.ErrMalformed
	}
	return ipc.WriteRecord(ipc.Identity{Epoch: s.epoch, CallID: req.CallId, RecordID: 0, Type: "Health"}, ipc.HealthTo(s.connector.Health(stream.Context())), stream.Send)
}

func (s *ConnectorChild) Capabilities(stream grpc.BidiStreamingServer[gen.RecordFragment, gen.RecordFragment]) error {
	v := new(gen.CapabilityScope)
	if err := receiveRecord(stream, s.epoch, 1, 0, "CapabilityScope", v); err != nil {
		return err
	}
	scope, err := ipc.ScopeFrom(v)
	if err != nil {
		return err
	}
	result := s.connector.Capabilities(stream.Context(), scope)
	return ipc.WriteRecord(ipc.Identity{Epoch: s.epoch, CallID: 1, RecordID: 0, Type: "CapabilityResult"}, ipc.CapabilityResultTo(result), stream.Send)
}

func (s *ConnectorChild) Models(stream grpc.BidiStreamingServer[gen.RecordFragment, gen.RecordFragment]) error {
	v := new(gen.ModelQuery)
	if err := receiveRecord(stream, s.epoch, 1, 0, "ModelQuery", v); err != nil {
		return err
	}
	query, err := ipc.ModelQueryFrom(v)
	if err != nil {
		return err
	}
	result, ge := s.connector.Models(stream.Context(), query, core.InvocationServices{})
	if ge != nil {
		return status.Error(13, "model query failed")
	}
	return ipc.WriteRecord(ipc.Identity{Epoch: s.epoch, CallID: 1, RecordID: 0, Type: "ModelsResult"}, ipc.ModelsResultTo(result), stream.Send)
}

func (s *ConnectorChild) EstimateUsage(stream grpc.BidiStreamingServer[gen.EstimateFrame, gen.RecordFragment]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	rec, ok := first.Payload.(*gen.EstimateFrame_Record)
	if !ok || rec.Record == nil {
		return ipc.ErrMalformed
	}
	q := new(gen.UsageQuery)
	a := ipc.NewAssembler(ipc.Identity{Epoch: s.epoch, CallID: 1, RecordID: 0, Type: "UsageQuery"})
	if err = a.Add(rec.Record); err != nil {
		return err
	}
	for !a.Complete() {
		f, e := stream.Recv()
		if e != nil {
			return e
		}
		r, ok := f.Payload.(*gen.EstimateFrame_Record)
		if !ok || r.Record == nil {
			return ipc.ErrMalformed
		}
		if err = a.Add(r.Record); err != nil {
			return err
		}
	}
	if err = a.Decode(q); err != nil {
		return err
	}
	var body []byte
	for {
		f, e := stream.Recv()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
		b, ok := f.Payload.(*gen.EstimateFrame_Body)
		if !ok {
			return ipc.ErrMalformed
		}
		data, e := ipc.BodyChunkFrom(b.Body)
		if e != nil {
			return e
		}
		body = append(body, data...)
		if q.MaxBodyBytes < 0 || int64(len(body)) > q.MaxBodyBytes {
			return ipc.ErrMalformed
		}
	}
	query, err := ipc.UsageQueryFrom(q, body)
	if err != nil {
		return err
	}
	result, ge := s.connector.EstimateUsage(stream.Context(), query, core.InvocationServices{})
	if ge != nil {
		return status.Error(13, "usage estimate failed")
	}
	return ipc.WriteRecord(ipc.Identity{Epoch: s.epoch, CallID: 1, RecordID: 0, Type: "EstimateResult"}, ipc.EstimateTo(result), stream.Send)
}

func (s *ConnectorChild) Authenticate(stream grpc.BidiStreamingServer[gen.RecordFragment, gen.RecordFragment]) error {
	first, err := stream.Recv()
	if err != nil || first == nil || first.InvocationId == "" || first.Epoch != s.epoch {
		return ipc.ErrMalformed
	}
	invocation := first.InvocationId
	req := new(gen.AuthRequest)
	a := ipc.NewAssembler(ipc.Identity{Epoch: s.epoch, CallID: 1, RecordID: 0, Type: "AuthRequest"})
	for {
		if first.InvocationId != invocation {
			return ipc.ErrMalformed
		}
		if err = a.Add(first); err != nil {
			return err
		}
		if a.Complete() {
			break
		}
		first, err = stream.Recv()
		if err != nil {
			return err
		}
	}
	if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
		if err == nil {
			return ipc.ErrMalformed
		}
		return err
	}
	if err = a.Decode(req); err != nil {
		return err
	}
	value, err := ipc.AuthRequestFrom(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(stream.Context())
	if err = s.flow.RegisterInvocation(invocation, cancel); err != nil {
		cancel()
		return err
	}
	defer func() { s.flow.UnregisterInvocation(invocation); cancel() }()
	services := core.InvocationServices{Transport: NewHTTPDoerClient(s.host, s.flow, s.epoch, invocation), Credentials: NewCredentialClient(s.host, s.epoch, invocation), Logger: s.invocationLogger(ctx, invocation)}
	result, ge := s.connector.Authenticate(ctx, value, services)
	if ge != nil {
		return status.Error(13, "authentication failed")
	}
	return ipc.WriteRecord(ipc.Identity{Epoch: s.epoch, CallID: 1, RecordID: 0, Type: "AuthResult"}, ipc.AuthResultTo(result), func(f *gen.RecordFragment) error { f.InvocationId = invocation; return stream.Send(f) })
}

func (s *ConnectorChild) Execute(stream grpc.BidiStreamingServer[gen.ExecuteFrame, gen.ExecuteFrame]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	r, ok := first.Payload.(*gen.ExecuteFrame_Record)
	if !ok || r.Record == nil || r.Record.RecordType != "ExecuteStart" {
		return ipc.ErrMalformed
	}
	invocation := r.Record.InvocationId
	if first.Epoch != s.epoch || first.CallId != 1 || invocation == "" || r.Record.InvocationId != invocation {
		return ipc.ErrMalformed
	}
	ctx, cancel := context.WithCancel(stream.Context())
	if err = s.flow.RegisterInvocation(invocation, cancel); err != nil {
		cancel()
		return err
	}
	defer func() { s.flow.UnregisterInvocation(invocation); cancel() }()
	start := new(gen.ExecuteStart)
	a := ipc.NewAssembler(ipc.Identity{Epoch: s.epoch, CallID: 1, RecordID: 0, Type: "ExecuteStart"})
	if err = validateResponsesExecuteFrame(ctx, s.flow, first, len(r.Record.Data)); err != nil {
		return err
	}
	if err = a.Add(r.Record); err != nil {
		return err
	}
	for !a.Complete() {
		f, e := stream.Recv()
		if e != nil {
			return e
		}
		if !responsesExecuteIdentity(f, s.epoch, invocation) {
			return ipc.ErrMalformed
		}
		x, ok := f.Payload.(*gen.ExecuteFrame_Record)
		if !ok || x.Record == nil {
			return ipc.ErrMalformed
		}
		if err = validateResponsesExecuteFrame(ctx, s.flow, f, len(x.Record.Data)); err != nil {
			return err
		}
		if err = a.Add(x.Record); err != nil {
			return err
		}
	}
	if err = a.Decode(start); err != nil {
		return err
	}
	scope, err := ipc.AttemptFrom(start.Scope)
	if err != nil {
		return err
	}
	scope.MaxBodyBytes = start.MaxBodyBytes
	var body []byte
	inputComplete := false
	for {
		f, e := stream.Recv()
		if errors.Is(e, io.EOF) {
			return ipc.ErrMalformed
		}
		if e != nil {
			return e
		}
		if !responsesExecuteIdentity(f, s.epoch, invocation) {
			return ipc.ErrMalformed
		}
		switch p := f.Payload.(type) {
		case *gen.ExecuteFrame_Body:
			b, x := ipc.BodyChunkFrom(p.Body)
			if x != nil {
				return x
			}
			body = append(body, b...)
			if int64(len(body)) > scope.MaxBodyBytes {
				return ipc.ErrMalformed
			}
			if x = validateResponsesExecuteFrame(ctx, s.flow, f, len(b)); x != nil {
				return x
			}
		case *gen.ExecuteFrame_Record:
			if p.Record == nil || p.Record.InvocationId != invocation || p.Record.RecordType != "InputComplete" || p.Record.RecordId != 1 {
				return ipc.ErrMalformed
			}
			var end gen.InputComplete
			if x := validateResponsesExecuteFrame(ctx, s.flow, f, len(p.Record.Data)); x != nil {
				return x
			}
			if x := ipc.Reassemble(ipc.Identity{Epoch: s.epoch, CallID: 1, RecordID: 1, Type: "InputComplete"}, []*gen.RecordFragment{p.Record}, &end); x != nil {
				return x
			}
			inputComplete = true
		default:
			return ipc.ErrMalformed
		}
		if inputComplete {
			if _, e := stream.Recv(); !errors.Is(e, io.EOF) {
				return ipc.ErrMalformed
			}
			break
		}
	}
	req, err := ipc.ExecutionRequestFrom(start.Request, body)
	if err != nil {
		return err
	}
	services := core.InvocationServices{Transport: NewHTTPDoerClient(s.host, s.flow, s.epoch, invocation), Credentials: NewCredentialClient(s.host, s.epoch, invocation), Logger: s.invocationLogger(ctx, invocation)}
	response, ge := s.connector.Execute(ctx, req, scope, services)
	if ge != nil {
		wire := ipc.GatewayErrorTo(ge)
		if err := sendResponsesExecuteRecord(stream, s.flow, ctx, s.epoch, invocation, 0, "GatewayError", wire); err != nil {
			return err
		}
		return s.flow.WaitDrained(ctx, invocation)
	}
	defer response.Stream.Close()
	frame, err := response.Stream.Next(ctx)
	if err != nil {
		return err
	}
	if frame.Type != core.FrameHead {
		return ipc.ErrMalformed
	}
	head, err := ipc.HeadTo(frame.Head)
	if err != nil {
		return err
	}
	if err = sendResponsesExecuteRecord(stream, s.flow, ctx, s.epoch, invocation, 0, "HeadFrame", head); err != nil {
		return err
	}
	for {
		frame, e := response.Stream.Next(ctx)
		if errors.Is(e, io.EOF) {
			return nil
		}
		if e != nil {
			return e
		}
		switch frame.Type {
		case core.FrameBody:
			chunk, x := ipc.BodyChunkTo(frame.Body.Data)
			if x != nil {
				return x
			}
			out := &gen.ExecuteFrame{Payload: &gen.ExecuteFrame_Body{Body: chunk}}
			if x = sendResponsesExecuteData(stream, s.flow, ctx, s.epoch, invocation, len(frame.Body.Data), out); x != nil {
				return x
			}
		case core.FrameComplete:
			complete := ipc.CompleteTo(frame.Complete)
			if err := sendResponsesExecuteRecord(stream, s.flow, ctx, s.epoch, invocation, 2, "CompleteFrame", complete); err != nil {
				return err
			}
			return s.flow.WaitDrained(ctx, invocation)
		}
	}
}

func (s *ConnectorChild) invocationLogger(ctx context.Context, invocation string) *slog.Logger {
	s.logMu.Lock()
	if s.logStopped {
		s.logMu.Unlock()
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.logWorker == nil {
		parent := s.logParent
		if parent == nil {
			parent = context.Background()
		}
		processCtx, cancel := context.WithCancel(parent)
		s.logCancel = cancel
		s.logWorker = NewLogDispatcher(processCtx, s.host, s.epoch)
	}
	worker := s.logWorker
	s.logMu.Unlock()
	return slog.New(NewLogHandler(ctx, worker, invocation))
}

func (s *ConnectorChild) setLogParent(ctx context.Context) {
	s.logMu.Lock()
	if s.logStopped {
		s.logMu.Unlock()
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.logParent = ctx
	old, oldCancel := s.logWorker, s.logCancel
	var replacement *LogDispatcher
	var cancel context.CancelFunc
	if old != nil {
		processCtx, stop := context.WithCancel(ctx)
		cancel = stop
		replacement = NewLogDispatcher(processCtx, s.host, s.epoch)
	}
	s.logWorker, s.logCancel = replacement, cancel
	s.logMu.Unlock()
	if oldCancel != nil {
		oldCancel()
	}
	if old != nil {
		<-old.done
	}
}

func (s *ConnectorChild) resetLogEpoch(epoch uint64) {
	s.logMu.Lock()
	s.epoch = epoch
	if s.logStopped {
		s.logMu.Unlock()
		return
	}
	parent := s.logParent
	old, oldCancel := s.logWorker, s.logCancel
	var replacement *LogDispatcher
	var cancel context.CancelFunc
	if old != nil {
		if parent == nil {
			parent = context.Background()
		}
		processCtx, stop := context.WithCancel(parent)
		cancel = stop
		replacement = NewLogDispatcher(processCtx, s.host, epoch)
	}
	s.logWorker, s.logCancel = replacement, cancel
	s.logMu.Unlock()
	if oldCancel != nil {
		oldCancel()
	}
	if old != nil {
		<-old.done
	}
}

func (s *ConnectorChild) stopLogger() {
	s.logMu.Lock()
	worker, cancel := s.logWorker, s.logCancel
	s.logWorker, s.logCancel = nil, nil
	s.logStopped = true
	s.logMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if worker != nil {
		<-worker.done
	}
}

func responsesExecuteIdentity(f *gen.ExecuteFrame, epoch uint64, invocation string) bool {
	return f != nil && f.Epoch == epoch && f.CallId == 1 && f.InvocationId == invocation
}

func validateResponsesExecuteFrame(ctx context.Context, flow *ChildFlowControl, f *gen.ExecuteFrame, size int) error {
	id := creditIdentity{epoch: f.Epoch, invocationID: f.InvocationId, operation: "Execute", callID: f.CallId, direction: "host_to_child", sequence: f.DataSequence}
	if err := flow.ValidateIncoming(id, size); err != nil {
		return err
	}
	return flow.AckIncoming(ctx, id)
}

func sendResponsesExecuteData(stream grpc.BidiStreamingServer[gen.ExecuteFrame, gen.ExecuteFrame], flow *ChildFlowControl, ctx context.Context, epoch uint64, invocation string, size int, frame *gen.ExecuteFrame) error {
	id, err := flow.ReserveNext(ctx, invocation, "Execute", 1, size)
	if err != nil {
		return err
	}
	frame.Epoch, frame.CallId, frame.InvocationId, frame.DataSequence = epoch, 1, invocation, id.sequence
	return stream.Send(frame)
}

func sendResponsesExecuteRecord(stream grpc.BidiStreamingServer[gen.ExecuteFrame, gen.ExecuteFrame], flow *ChildFlowControl, ctx context.Context, epoch uint64, invocation string, record uint64, typ string, value protoreflect.ProtoMessage) error {
	return ipc.WriteRecord(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: record, Type: typ}, value, func(f *gen.RecordFragment) error {
		f.InvocationId = invocation
		return sendResponsesExecuteData(stream, flow, ctx, epoch, invocation, len(f.Data), &gen.ExecuteFrame{Payload: &gen.ExecuteFrame_Record{Record: f}})
	})
}

func (s *ConnectorChild) Close(ctx context.Context, _ *gen.RecordFragment) (*gen.RecordFragment, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return &gen.RecordFragment{Epoch: s.epoch, CallId: 1}, nil
	}
	s.closed = true
	s.mu.Unlock()
	s.stopLogger()
	err := s.connector.Close(ctx)
	if s.stop != nil {
		go s.stop()
	}
	return &gen.RecordFragment{Epoch: s.epoch, CallId: 1}, err
}
func (s *ConnectorChild) FlowControl(ctx context.Context, req *gen.FlowControlRequest) (*gen.FlowControlResponse, error) {
	return s.flow.FlowControl(ctx, req)
}

func receiveRecord(stream interface {
	Recv() (*gen.RecordFragment, error)
}, epoch, call, record uint64, typ string, dst interface{ ProtoReflect() protoreflect.Message }) error {
	a := ipc.NewAssembler(ipc.Identity{Epoch: epoch, CallID: call, RecordID: record, Type: typ})
	for {
		f, e := stream.Recv()
		if e != nil {
			return e
		}
		if err := a.Add(f); err != nil {
			return err
		}
		if a.Complete() {
			if _, e = stream.Recv(); !errors.Is(e, io.EOF) {
				if e == nil {
					return ipc.ErrMalformed
				}
				return e
			}
			return a.Decode(dst)
		}
	}
}

// RunResponsesChild binds only the private IPC socket; startup configuration is
// intentionally outside M6-024.
func RunResponsesChild(ctx context.Context, connector *ConnectorChild, socket string) error {
	if connector == nil || !filepath.IsAbs(socket) {
		return errors.New("invalid child socket")
	}
	connector.setLogParent(ctx)
	defer connector.stopLogger()
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer func() { listener.Close(); os.Remove(socket) }()
	if err = os.Chmod(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(ipc.MaxGRPCMessageBytes), grpc.MaxSendMsgSize(ipc.MaxGRPCMessageBytes))
	connector.stop = server.GracefulStop
	gen.RegisterConnectorServer(server, connector)
	go func() { <-ctx.Done(); server.GracefulStop() }()
	err = server.Serve(listener)
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// RunNativeResponsesChild is the production child composition entry point. The
// parent owns invocation authority and both socket paths; neither is config.
func RunNativeResponsesChild(ctx context.Context) error {
	return RunConnectorChild(ctx, responses.NewConnector())
}

// RunConnectorChild composes a connector with invocation-scoped host services
// and the private child socket. It passes no gateway config or credentials.
func RunConnectorChild(ctx context.Context, implementation core.Connector) error {
	connectorSocket, hostSocket := os.Getenv(connectorSocketEnv), os.Getenv(hostServicesSocketEnv)
	if connectorSocket == "" || hostSocket == "" || !filepath.IsAbs(connectorSocket) || !filepath.IsAbs(hostSocket) {
		return errors.New("child IPC endpoints are required")
	}
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", hostSocket)
	}
	conn, err := grpc.NewClient("passthrough:///host-services", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(ipc.MaxGRPCMessageBytes), grpc.MaxCallSendMsgSize(ipc.MaxGRPCMessageBytes)), grpc.WithContextDialer(dial))
	if err != nil {
		return err
	}
	defer conn.Close()
	flow, controlConn, err := DialChildFlowControl(ctx, hostSocket, 1)
	if err != nil {
		return err
	}
	defer controlConn.Close()
	child, err := NewConnectorChild(implementation, gen.NewHostServicesClient(conn), flow, flow.epoch)
	if err != nil {
		return err
	}
	return RunResponsesChild(ctx, child, connectorSocket)
}

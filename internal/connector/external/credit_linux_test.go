//go:build linux

package external

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

func TestM6BackpressureBrokerAggregateAndAckResume(t *testing.T) {
	broker := newCreditBroker(9)
	ids := make([]creditIdentity, 0, maxQueueFramesProcess)
	for invocation := range 4 {
		name := string(rune('a' + invocation))
		if err := broker.register(name); err != nil {
			t.Fatal(err)
		}
		for sequence := range maxQueueFramesPerInvocation {
			id := creditIdentity{epoch: 9, invocationID: name, operation: "Execute", callID: 1, direction: "child_to_host", sequence: uint64(sequence + 1)}
			if err := broker.reserve(context.Background(), id, 64<<10); err != nil {
				t.Fatal(err)
			}
			if err := broker.delivered(id, 64<<10); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
	}
	if frames, bytes := broker.usage(); frames != 256 || bytes != 16<<20 {
		t.Fatalf("usage=(%d frames,%d bytes), want (256,16 MiB)", frames, bytes)
	}
	blocked := make(chan error, 1)
	next := creditIdentity{epoch: 9, invocationID: "a", operation: "HTTPDo", callID: 2, direction: "host_to_child", sequence: 1}
	go func() { blocked <- broker.reserve(context.Background(), next, 100) }()
	select {
	case err := <-blocked:
		t.Fatalf("reservation passed full process/frame ceiling: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := broker.ack([]creditIdentity{ids[0]}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ACK did not release credit to the blocked cross-operation sender")
	}
	if err := broker.delivered(next, 100); err != nil {
		t.Fatal(err)
	}
	if err := broker.ack([]creditIdentity{next}); err != nil {
		t.Fatal(err)
	}
	broker.revoke("a")
	if frames, bytes := broker.usage(); frames != 192 || bytes != 12<<20 {
		t.Fatalf("revoke did not reclaim outstanding reservations: (%d,%d)", frames, bytes)
	}
}

func TestM6BackpressureBrokerTinyFrameAndInvalidAck(t *testing.T) {
	broker := newCreditBroker(3)
	if err := broker.register("invoke"); err != nil {
		t.Fatal(err)
	}
	for sequence := range maxQueueFramesPerInvocation {
		id := creditIdentity{epoch: 3, invocationID: "invoke", operation: "Execute", callID: 1, direction: "child_to_host", sequence: uint64(sequence + 1)}
		if err := broker.reserve(context.Background(), id, 100); err != nil {
			t.Fatal(err)
		}
		if err := broker.delivered(id, 100); err != nil {
			t.Fatal(err)
		}
	}
	if frames, bytes := broker.usage(); frames != 64 || bytes != 6400 {
		t.Fatalf("tiny frames used (%d,%d), want (64,6400)", frames, bytes)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	blocked := creditIdentity{epoch: 3, invocationID: "invoke", operation: "Execute", callID: 1, direction: "child_to_host", sequence: 65}
	if err := broker.reserve(ctx, blocked, 100); err != context.DeadlineExceeded {
		t.Fatalf("65th tiny frame error=%v, want deadline", err)
	}
	if err := broker.ack([]creditIdentity{{epoch: 3, invocationID: "invoke", operation: "Execute", callID: 1, direction: "child_to_host", sequence: 1}, {epoch: 3, invocationID: "invoke", operation: "Execute", callID: 1, direction: "child_to_host", sequence: 1}}); err == nil {
		t.Fatal("duplicate coalesced ACK was accepted")
	}
}

func TestM6BackpressureJointBidirectional(t *testing.T) {
	broker := newCreditBroker(31)
	if err := broker.register("joint"); err != nil {
		t.Fatal(err)
	}
	ids := make([]creditIdentity, 0, maxQueueFramesPerInvocation)
	for sequence := range maxQueueFramesPerInvocation / 2 {
		for _, stream := range []creditStream{
			{invocationID: "joint", operation: "Execute", callID: 1, direction: "child_to_host"},
			{invocationID: "joint", operation: "HTTPDo", callID: 2, direction: "host_to_child"},
		} {
			id := creditIdentity{epoch: 31, invocationID: stream.invocationID, operation: stream.operation, callID: stream.callID, direction: stream.direction, sequence: uint64(sequence + 1)}
			if err := broker.reserve(context.Background(), id, 64<<10); err != nil {
				t.Fatal(err)
			}
			if err := broker.delivered(id, 64<<10); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
	}
	if frames, bytes := broker.usage(); frames != 64 || bytes != 4<<20 {
		t.Fatalf("joint usage=(%d,%d), want (64,4 MiB)", frames, bytes)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	extra := creditIdentity{epoch: 31, invocationID: "joint", operation: "HTTPDo", callID: 3, direction: "child_to_host", sequence: 1}
	if err := broker.reserve(ctx, extra, 1); err != context.DeadlineExceeded {
		t.Fatalf("joint bidirectional 65th frame error=%v, want deadline", err)
	}
	if err := broker.ack([]creditIdentity{ids[0]}); err != nil {
		t.Fatal(err)
	}
	if err := broker.reserve(context.Background(), extra, 1); err != nil {
		t.Fatalf("ACK did not release joint-stream credit: %v", err)
	}
}

func TestM6BackpressureBrokerRejectsStaleReplayAndRevokesSaturatedWaiter(t *testing.T) {
	broker := newCreditBroker(11)
	if err := broker.register("invoke"); err != nil {
		t.Fatal(err)
	}
	id := creditIdentity{epoch: 11, invocationID: "invoke", operation: "HTTPDo", callID: 3, direction: "child_to_host", sequence: 1}
	if err := broker.reserve(context.Background(), id, 100); err != nil {
		t.Fatal(err)
	}
	if err := broker.reserve(context.Background(), id, 100); err == nil {
		t.Fatal("replayed reservation was accepted")
	}
	stale := id
	stale.epoch++
	if err := broker.delivered(stale, 100); err == nil {
		t.Fatal("stale epoch was accepted")
	}
	if err := broker.delivered(id, 99); err == nil {
		t.Fatal("reservation with mismatched byte charge was accepted")
	}
	if err := broker.delivered(id, 100); err != nil {
		t.Fatal(err)
	}
	if err := broker.ack([]creditIdentity{id}); err != nil {
		t.Fatal(err)
	}

	for sequence := range maxQueueFramesPerInvocation {
		id := creditIdentity{epoch: 11, invocationID: "invoke", operation: "Execute", callID: 4, direction: "child_to_host", sequence: uint64(sequence + 1)}
		if err := broker.reserve(context.Background(), id, 1); err != nil {
			t.Fatal(err)
		}
	}
	next := creditIdentity{epoch: 11, invocationID: "invoke", operation: "Execute", callID: 4, direction: "child_to_host", sequence: maxQueueFramesPerInvocation + 1}
	result := make(chan error, 1)
	go func() { result <- broker.reserve(context.Background(), next, 1) }()
	time.Sleep(10 * time.Millisecond)
	broker.revoke("invoke")
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("saturated reservation survived invocation revocation")
		}
	case <-time.After(time.Second):
		t.Fatal("revocation did not wake saturated sender")
	}
	if frames, bytes := broker.usage(); frames != 0 || bytes != 0 {
		t.Fatalf("revocation leaked reservations: (%d,%d)", frames, bytes)
	}
}

func TestM6BackpressureControlLaneReservesUrgentCapacity(t *testing.T) {
	lane := newControlLane()
	var release []func()
	for range controlSlots[controlReserve] {
		done, err := lane.acquire(context.Background(), controlReserve)
		if err != nil {
			t.Fatal(err)
		}
		release = append(release, done)
	}
	urgent, err := lane.acquire(context.Background(), controlUrgent)
	if err != nil {
		t.Fatalf("urgent control blocked by Reserve saturation: %v", err)
	}
	ack, err := lane.acquire(context.Background(), controlAck)
	if err != nil {
		t.Fatalf("ACK blocked by Reserve saturation: %v", err)
	}
	urgent()
	ack()
	for _, done := range release {
		done()
	}
}

type ackRecordingCaller struct{ acknowledgements []uint64 }

func (c *ackRecordingCaller) FlowControl(_ context.Context, req *gen.FlowControlRequest, _ ...grpc.CallOption) (*gen.FlowControlResponse, error) {
	for _, id := range req.Acknowledgements {
		c.acknowledgements = append(c.acknowledgements, id.Sequence)
	}
	return &gen.FlowControlResponse{Kind: gen.FlowControlKind_FLOW_CONTROL_ACK}, nil
}

type frameClientStream struct {
	frames []*gen.HTTPFrame
	ctx    context.Context
}

func (s *frameClientStream) Send(*gen.HTTPFrame) error { return nil }
func (s *frameClientStream) Recv() (*gen.HTTPFrame, error) {
	if len(s.frames) == 0 {
		return nil, io.EOF
	}
	f := s.frames[0]
	s.frames = s.frames[1:]
	return f, nil
}
func (s *frameClientStream) Header() (metadata.MD, error) { return nil, nil }
func (s *frameClientStream) Trailer() metadata.MD         { return nil }
func (s *frameClientStream) CloseSend() error             { return nil }
func (s *frameClientStream) Context() context.Context     { return s.ctx }
func (s *frameClientStream) SendMsg(any) error            { return nil }
func (s *frameClientStream) RecvMsg(any) error            { return io.EOF }

type executeFrameClientStream struct {
	frame *gen.ExecuteFrame
	ctx   context.Context
}

func (s *executeFrameClientStream) Send(*gen.ExecuteFrame) error { return nil }
func (s *executeFrameClientStream) Recv() (*gen.ExecuteFrame, error) {
	f := s.frame
	s.frame = nil
	return f, nil
}
func (s *executeFrameClientStream) Header() (metadata.MD, error) { return nil, nil }
func (s *executeFrameClientStream) Trailer() metadata.MD         { return nil }
func (s *executeFrameClientStream) CloseSend() error             { return nil }
func (s *executeFrameClientStream) Context() context.Context     { return s.ctx }
func (s *executeFrameClientStream) SendMsg(any) error            { return nil }
func (s *executeFrameClientStream) RecvMsg(any) error            { return io.EOF }

type blockingControlServer struct {
	gen.UnimplementedConnectorServer
	entered chan struct{}
}

func (c *blockingControlServer) FlowControl(ctx context.Context, req *gen.FlowControlRequest) (*gen.FlowControlResponse, error) {
	if req.Kind == gen.FlowControlKind_FLOW_CONTROL_CANCEL {
		return &gen.FlowControlResponse{Kind: req.Kind, Identity: req.Identity}, nil
	}
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestM6BackpressurePartialReads(t *testing.T) {
	caller := &ackRecordingCaller{}
	flow, err := NewChildFlowControl(81, caller)
	if err != nil {
		t.Fatal(err)
	}
	bodyID := creditIdentity{epoch: 81, invocationID: "invoke", operation: "HTTPDo", callID: 7, direction: "host_to_child", sequence: 1}
	completeID := bodyID
	completeID.sequence = 2
	for _, grant := range []struct {
		id   creditIdentity
		size int
	}{{bodyID, 100}, {completeID, 0}} {
		if _, err := flow.FlowControl(context.Background(), &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_GRANT, Identity: grant.id.wire(), Frames: 1, ApplicationBytes: uint32(grant.size)}); err != nil {
			t.Fatal(err)
		}
	}
	var completeRecord *gen.RecordFragment
	if err := ipc.WriteRecord(ipc.Identity{Epoch: 81, CallID: 7, RecordID: 1, Type: "HTTPBodyComplete"}, &gen.HTTPBodyComplete{}, func(f *gen.RecordFragment) error {
		f.InvocationId = "invoke"
		completeRecord = f
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	frames := []*gen.HTTPFrame{
		{Epoch: 81, CallId: 7, InvocationId: "invoke", DataSequence: 1, Payload: &gen.HTTPFrame_Body{Body: &gen.BodyChunk{Data: make([]byte, 100)}}},
		{Epoch: 81, CallId: 7, InvocationId: "invoke", DataSequence: 2, Payload: &gen.HTTPFrame_Record{Record: completeRecord}},
	}
	body := &httpResponseStreamBody{stream: &frameClientStream{frames: frames, ctx: context.Background()}, flow: flow, ctx: context.Background(), epoch: 81, callID: 7, invocationID: "invoke"}
	part := make([]byte, 10)
	if n, err := body.Read(part); err != nil || n != len(part) {
		t.Fatalf("first partial read=(%d,%v)", n, err)
	}
	if len(caller.acknowledgements) != 0 {
		t.Fatal("body credit ACKed before the complete chunk was delivered")
	}
	rest := make([]byte, 90)
	if n, err := body.Read(rest); err != nil || n != len(rest) {
		t.Fatalf("final partial read=(%d,%v)", n, err)
	}
	if len(caller.acknowledgements) != 1 || caller.acknowledgements[0] != bodyID.sequence {
		t.Fatalf("ACKs after body delivery=%v, want only sequence 1", caller.acknowledgements)
	}
	if n, err := body.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("terminal read=(%d,%v), want EOF", n, err)
	}
	if len(caller.acknowledgements) != 2 || caller.acknowledgements[1] != completeID.sequence {
		t.Fatalf("final ACKs=%v, want sequences 1 and 2", caller.acknowledgements)
	}
}

func TestM6BackpressureExecuteAckHonorsNextCancellation(t *testing.T) {
	const epoch = 83
	registry := newInvocationRegistry(epoch)
	if err := registry.credits.register("invoke"); err != nil {
		t.Fatal(err)
	}
	id := creditIdentity{epoch: epoch, invocationID: "invoke", operation: "Execute", callID: 1, direction: "child_to_host", sequence: 1}
	if err := registry.credits.reserve(context.Background(), id, 100); err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	caller := &blockingControlServer{entered: make(chan struct{}, 2)}
	gen.RegisterConnectorServer(server, caller)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///execute-ack-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	connector := &Connector{epoch: epoch, control: gen.NewConnectorClient(conn), invocations: registry}
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	closeCtx, closeCancel := context.WithCancel(context.Background())
	defer closeCancel()
	stream := &executeResponseStream{
		connector: connector,
		stream: &executeFrameClientStream{
			frame: &gen.ExecuteFrame{Epoch: epoch, CallId: 1, InvocationId: "invoke", DataSequence: 1, Payload: &gen.ExecuteFrame_Body{Body: &gen.BodyChunk{Data: make([]byte, 100)}}},
			ctx:   parent,
		},
		epoch: epoch, invocationID: "invoke", ctx: parent, closeCtx: closeCtx, closeCancel: closeCancel, cleanup: func() {},
	}
	callerCtx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := stream.Next(callerCtx); result <- err }()
	select {
	case <-caller.entered:
	case <-time.After(time.Second):
		select {
		case err := <-result:
			t.Fatalf("Next returned before ACK RPC: %v", err)
		default:
			t.Fatal("Next did not enter the stalled ACK RPC")
		}
	}
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("Next error=%v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Next stayed blocked after caller cancellation during ACK")
	}

	if err := registry.credits.register("invoke-close"); err != nil {
		t.Fatal(err)
	}
	closeID := creditIdentity{epoch: epoch, invocationID: "invoke-close", operation: "Execute", callID: 1, direction: "child_to_host", sequence: 1}
	if err := registry.credits.reserve(context.Background(), closeID, 100); err != nil {
		t.Fatal(err)
	}
	closeContext, closeStreamCancel := context.WithCancel(context.Background())
	defer closeStreamCancel()
	closeSignalCtx, closeSignalCancel := context.WithCancel(context.Background())
	defer closeSignalCancel()
	stream = &executeResponseStream{
		connector: connector,
		stream: &executeFrameClientStream{
			frame: &gen.ExecuteFrame{Epoch: epoch, CallId: 1, InvocationId: "invoke-close", DataSequence: 1, Payload: &gen.ExecuteFrame_Body{Body: &gen.BodyChunk{Data: make([]byte, 100)}}},
			ctx:   closeContext,
		},
		epoch: epoch, invocationID: "invoke-close", ctx: closeContext, closeCtx: closeSignalCtx, closeCancel: closeSignalCancel, cleanup: func() {},
	}
	result = make(chan error, 1)
	go func() { _, err := stream.Next(context.Background()); result <- err }()
	select {
	case <-caller.entered:
	case <-time.After(time.Second):
		t.Fatal("Next did not enter stalled ACK RPC before explicit Close")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("Next after Close error=%v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("explicit Close did not interrupt stalled ACK")
	}
}

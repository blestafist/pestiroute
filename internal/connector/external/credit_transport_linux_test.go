//go:build linux

package external

import (
	"context"
	"net"
	"testing"
	"time"

	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const creditTransportEpoch = 71

type creditTransportConnector struct {
	gen.UnimplementedConnectorServer
	connector *Connector
	flow      *ChildFlowControl
	sent      chan struct{}
	done      chan struct{}
}

func (s *creditTransportConnector) FlowControl(ctx context.Context, req *gen.FlowControlRequest) (*gen.FlowControlResponse, error) {
	return s.flow.FlowControl(ctx, req)
}

func (s *creditTransportConnector) Execute(stream grpc.BidiStreamingServer[gen.ExecuteFrame, gen.ExecuteFrame]) error {
	defer func() { s.done <- struct{}{} }()
	var sequence uint64
	for range 33 {
		frame := &gen.ExecuteFrame{Payload: &gen.ExecuteFrame_Body{Body: &gen.BodyChunk{Data: make([]byte, 64<<10)}}}
		if err := s.connector.sendExecuteData(stream.Context(), "shared", 1, &sequence, 64<<10, frame, stream.Send); err != nil {
			return err
		}
		s.sent <- struct{}{}
	}
	return nil
}

type creditTransportHTTP struct {
	gen.UnimplementedHostServicesServer
	services *hostServices
	sent     chan struct{}
	done     chan struct{}
}

type flowOnlyConnectorServer struct {
	gen.UnimplementedConnectorServer
	flow *ChildFlowControl
}

func (s *flowOnlyConnectorServer) FlowControl(ctx context.Context, req *gen.FlowControlRequest) (*gen.FlowControlResponse, error) {
	return s.flow.FlowControl(ctx, req)
}

func (s *creditTransportHTTP) FlowControl(ctx context.Context, req *gen.FlowControlRequest) (*gen.FlowControlResponse, error) {
	return s.services.FlowControl(ctx, req)
}

func (s *creditTransportHTTP) HTTPDo(stream grpc.BidiStreamingServer[gen.HTTPFrame, gen.HTTPFrame]) error {
	defer func() { s.done <- struct{}{} }()
	request := &gen.HTTPFrame{Epoch: creditTransportEpoch, CallId: 2, InvocationId: "shared"}
	var sequence uint64
	for range 33 {
		frame := &gen.HTTPFrame{Payload: &gen.HTTPFrame_Body{Body: &gen.BodyChunk{Data: make([]byte, 64<<10)}}}
		if err := s.services.sendHTTPData(stream.Context(), stream, request, &sequence, 64<<10, frame); err != nil {
			return err
		}
		s.sent <- struct{}{}
	}
	return nil
}

func TestM6BackpressureLargeWindowEnforcement(t *testing.T) {
	registry := newInvocationRegistry(creditTransportEpoch)
	if err := registry.credits.register("shared"); err != nil {
		t.Fatal(err)
	}
	connectorListener := bufconn.Listen(32 << 20)
	connectorServer := grpc.NewServer(grpc.MaxSendMsgSize(84<<10), grpc.MaxRecvMsgSize(84<<10))
	connectorService := &creditTransportConnector{sent: make(chan struct{}, 66), done: make(chan struct{}, 1)}
	gen.RegisterConnectorServer(connectorServer, connectorService)
	go func() { _ = connectorServer.Serve(connectorListener) }()
	defer connectorServer.Stop()
	connectorDial := func(context.Context, string) (net.Conn, error) { return connectorListener.Dial() }
	controlConn, err := grpc.NewClient("passthrough:///control", grpc.WithContextDialer(connectorDial), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(84<<10), grpc.MaxCallSendMsgSize(84<<10)))
	if err != nil {
		t.Fatal(err)
	}
	defer controlConn.Close()
	connectorService.connector = &Connector{epoch: creditTransportEpoch, control: gen.NewConnectorClient(controlConn), invocations: registry}

	hostListener := bufconn.Listen(32 << 20)
	hostServer := grpc.NewServer(grpc.MaxSendMsgSize(84<<10), grpc.MaxRecvMsgSize(84<<10))
	hostTransport := &creditTransportHTTP{services: &hostServices{registry: registry, control: gen.NewConnectorClient(controlConn)}, sent: make(chan struct{}, 66), done: make(chan struct{}, 1)}
	gen.RegisterHostServicesServer(hostServer, hostTransport)
	go func() { _ = hostServer.Serve(hostListener) }()
	defer hostServer.Stop()
	hostControlConn, err := grpc.NewClient("passthrough:///host-control", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return hostListener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(84<<10), grpc.MaxCallSendMsgSize(84<<10)))
	if err != nil {
		t.Fatal(err)
	}
	defer hostControlConn.Close()
	childFlow, err := NewChildFlowControl(creditTransportEpoch, gen.NewHostServicesClient(hostControlConn))
	if err != nil {
		t.Fatal(err)
	}
	connectorService.flow = childFlow
	if err := childFlow.RegisterInvocation("shared", func() {}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dataConn, err := grpc.NewClient("passthrough:///connector", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return connectorListener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithInitialWindowSize(32<<20), grpc.WithInitialConnWindowSize(32<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer dataConn.Close()
	httpConn, err := grpc.NewClient("passthrough:///host", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return hostListener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithInitialWindowSize(32<<20), grpc.WithInitialConnWindowSize(32<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer httpConn.Close()
	executeStream, err := gen.NewConnectorClient(dataConn).Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	httpStream, err := gen.NewHostServicesClient(httpConn).HTTPDo(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for range maxQueueFramesPerInvocation {
		select {
		case <-connectorService.sent:
		case <-hostTransport.sent:
		case <-ctx.Done():
			t.Fatalf("only %d frames sent before timeout", maxQueueFramesPerInvocation)
		}
	}
	if frames, bytes := registry.credits.usage(); frames != 64 || bytes != 4<<20 {
		t.Fatalf("shared credits=(%d,%d), want (64,4 MiB)", frames, bytes)
	}
	select {
	case <-connectorService.sent:
		t.Fatal("Execute sent beyond the shared invocation cap")
	case <-hostTransport.sent:
		t.Fatal("reverse HTTP sent beyond the shared invocation cap")
	case <-time.After(30 * time.Millisecond):
	}
	frame, err := executeStream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	id := creditIdentity{epoch: frame.Epoch, invocationID: frame.InvocationId, operation: "Execute", callID: frame.CallId, direction: "host_to_child", sequence: frame.DataSequence}
	if err := childFlow.ValidateIncoming(id, len(frame.GetBody().Data)); err != nil {
		t.Fatal(err)
	}
	if err := childFlow.AckIncoming(ctx, id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connectorService.sent:
	case <-hostTransport.sent:
	case <-ctx.Done():
		t.Fatal("consumption ACK did not release shared credit")
	}
	// Keep the reverse HTTP stream live as a distinct transport connection.
	_ = httpStream
	cancel()
	for range 2 {
		select {
		case <-connectorService.done:
		case <-hostTransport.done:
		case <-time.After(time.Second):
			t.Fatal("cancel under saturated shared credits did not stop sender promptly")
		}
	}
	registry.credits.revoke("shared")
	if frames, bytes := registry.credits.usage(); frames != 0 || bytes != 0 {
		t.Fatalf("cancel left broker credits outstanding: (%d,%d)", frames, bytes)
	}
}

func TestM6BackpressureUrgentControlsBypassSaturatedDataAndReserveLane(t *testing.T) {
	const epoch = 72
	registry := newInvocationRegistry(epoch)
	invocationID, _, err := registry.begin(context.Background(), "account")
	if err != nil {
		t.Fatal(err)
	}
	lease, _, _, err := registry.admitCallback(epoch, invocationID, 900)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	hostListener := bufconn.Listen(4 << 20)
	hostServer := grpc.NewServer()
	services := &hostServices{registry: registry}
	gen.RegisterHostServicesServer(hostServer, services)
	go func() { _ = hostServer.Serve(hostListener) }()
	defer hostServer.Stop()
	hostDial := func(context.Context, string) (net.Conn, error) { return hostListener.Dial() }
	hostConn, err := grpc.NewClient("passthrough:///host-control", grpc.WithContextDialer(hostDial), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer hostConn.Close()
	childFlow, err := NewChildFlowControl(epoch, gen.NewHostServicesClient(hostConn))
	if err != nil {
		t.Fatal(err)
	}
	if err := childFlow.RegisterInvocation(invocationID, func() {}); err != nil {
		t.Fatal(err)
	}
	for range maxQueueFramesPerInvocation {
		if _, err := childFlow.ReserveNext(context.Background(), invocationID, "HTTPDo", 900, 64<<10); err != nil {
			t.Fatal(err)
		}
	}

	childListener := bufconn.Listen(1 << 20)
	childServer := grpc.NewServer()
	gen.RegisterConnectorServer(childServer, &flowOnlyConnectorServer{flow: childFlow})
	go func() { _ = childServer.Serve(childListener) }()
	defer childServer.Stop()
	childConn, err := grpc.NewClient("passthrough:///child-control", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return childListener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer childConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 80)
	startReserves := func(firstCallID, count int) {
		for i := range count {
			callID := uint64(firstCallID + i)
			go func() {
				_, err := childFlow.ReserveNext(ctx, invocationID, "HTTPDo", callID, 64<<10)
				results <- err
			}()
		}
	}
	waitSaturated := func() {
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			if len(registry.control.slots[controlReserve]) == controlSlots[controlReserve] && len(childFlow.lane.slots[controlReserve]) == controlSlots[controlReserve] {
				inflight := 0
				capacity := 0
				for i := range controlSlots {
					inflight += len(registry.control.slots[i]) + len(childFlow.lane.slots[i])
					capacity += cap(registry.control.slots[i]) + cap(childFlow.lane.slots[i])
				}
				if inflight < 2*controlSlots[controlReserve] || capacity != maxControlMessages {
					t.Fatalf("two-endpoint control budget=(%d in-flight,%d capacity), want saturated reserves and capacity %d", inflight, capacity, maxControlMessages)
				}
				if capacity*maxControlMessageBytes > maxControlBytes {
					t.Fatalf("control byte capacity=%d exceeds %d", capacity*maxControlMessageBytes, maxControlBytes)
				}
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("timed out saturating reserve lanes")
			case <-deadline.C:
				t.Fatal("reserve control queues did not saturate")
			case <-tick.C:
			}
		}
	}

	startReserves(1000, controlSlots[controlReserve])
	waitSaturated()
	start := time.Now()
	if err := childFlow.CancelCall(invocationID, "HTTPDo", 900); err != nil {
		t.Fatalf("urgent child→host CANCEL blocked behind reserve/data saturation: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("urgent CANCEL exceeded one second under saturation")
	}
	for range controlSlots[controlReserve] {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("released reserve failed: %v", err)
			}
		case <-ctx.Done():
			frames, bytes := registry.credits.usage()
			t.Fatalf("reserve did not resume after urgent CANCEL: usage=(%d,%d) hostReserve=%d childReserve=%d", frames, bytes, len(registry.control.slots[controlReserve]), len(childFlow.lane.slots[controlReserve]))
		}
	}
	if frames, bytes := registry.credits.usage(); frames != controlSlots[controlReserve] || bytes != controlSlots[controlReserve]*(64<<10) {
		t.Fatalf("post-cancel reservations=(%d,%d)", frames, bytes)
	}

	// Fill the invocation queue, then saturate both reserve lanes again. Host
	// CLOSE uses its reserved urgent slot on the same shared host lane.
	for i := 0; i < maxQueueFramesPerInvocation-controlSlots[controlReserve]; i++ {
		id := creditIdentity{epoch: epoch, invocationID: invocationID, operation: "HTTPDo", callID: uint64(2000 + i), direction: "child_to_host", sequence: 1}
		if err := registry.credits.reserve(context.Background(), id, 64<<10); err != nil {
			t.Fatal(err)
		}
	}
	startReserves(3000, controlSlots[controlReserve])
	waitSaturated()
	closeID := &gen.FlowIdentity{Epoch: epoch, InvocationId: "process", Operation: "Execute", CallId: 1, Direction: "host_to_child", Sequence: 1}
	start = time.Now()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	_, err = controlCall(closeCtx, registry.control, gen.NewConnectorClient(childConn), controlUrgent, &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_CLOSE, Identity: closeID})
	closeCancel()
	if err != nil {
		t.Fatalf("urgent host→child CLOSE blocked behind reserve/data saturation: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("urgent CLOSE exceeded one second under saturation")
	}
	cancel()
	for range controlSlots[controlReserve] {
		select {
		case <-results:
		case <-time.After(time.Second):
			t.Fatal("cancellation did not release saturated reserve RPCs")
		}
	}
	registry.close()
	if frames, bytes := registry.credits.usage(); frames != 0 || bytes != 0 {
		t.Fatalf("shutdown leaked data reservations: (%d,%d)", frames, bytes)
	}
}

func TestM6BackpressureChildControlRejectsMalformedIdentityAndReplay(t *testing.T) {
	const epoch = 73
	flow, err := NewChildFlowControl(epoch, unusedControlCaller{})
	if err != nil {
		t.Fatal(err)
	}
	canceled := 0
	if err := flow.RegisterInvocation("live", func() { canceled++ }); err != nil {
		t.Fatal(err)
	}
	reservation := creditIdentity{epoch: epoch, invocationID: "live", operation: "Execute", callID: 1, direction: "host_to_child", sequence: 7}
	flow.inbound[reservation] = 12
	flow.outbound[reservation] = struct{}{}

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	gen.RegisterConnectorServer(server, &flowOnlyConnectorServer{flow: flow})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///child-control-validation",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := gen.NewConnectorClient(conn)
	call := func(kind gen.FlowControlKind, id creditIdentity) error {
		_, err := client.FlowControl(context.Background(), &gen.FlowControlRequest{Kind: kind, Identity: id.wire()})
		return err
	}
	assertUnchanged := func() {
		t.Helper()
		if canceled != 0 || flow.cancel["live"] == nil || len(flow.inbound) != 1 || len(flow.outbound) != 1 || flow.closed {
			t.Fatalf("invalid control mutated child state: canceled=%d cancelRegistered=%t inbound=%d outbound=%d closed=%t", canceled, flow.cancel["live"] != nil, len(flow.inbound), len(flow.outbound), flow.closed)
		}
	}
	validCancel := creditIdentity{epoch: epoch, invocationID: "live", operation: "Execute", callID: 1, direction: "host_to_child", sequence: 1}
	for _, invalid := range []creditIdentity{
		{epoch: epoch, invocationID: "live", operation: "HTTPDo", callID: 1, direction: "host_to_child", sequence: 1},
		{epoch: epoch, invocationID: "live", operation: "Execute", callID: 2, direction: "host_to_child", sequence: 1},
		{epoch: epoch, invocationID: "live", operation: "Execute", callID: 1, direction: "child_to_host", sequence: 1},
		{epoch: epoch, invocationID: "live", operation: "Execute", callID: 1, direction: "host_to_child", sequence: 2},
	} {
		if err := call(gen.FlowControlKind_FLOW_CONTROL_CANCEL, invalid); err == nil {
			t.Fatalf("invalid CANCEL identity accepted: %+v", invalid)
		}
		assertUnchanged()
	}
	if err := call(gen.FlowControlKind_FLOW_CONTROL_CANCEL, validCancel); err != nil {
		t.Fatalf("valid CANCEL rejected: %v", err)
	}
	if canceled != 1 || flow.cancel["live"] != nil || len(flow.inbound) != 0 || len(flow.outbound) != 0 {
		t.Fatal("valid CANCEL did not revoke the live invocation exactly once")
	}
	if err := call(gen.FlowControlKind_FLOW_CONTROL_CANCEL, validCancel); err == nil {
		t.Fatal("replayed CANCEL accepted")
	}
	if canceled != 1 || flow.closed {
		t.Fatal("replayed CANCEL mutated child state")
	}

	if err := flow.RegisterInvocation("still-live", func() { canceled++ }); err != nil {
		t.Fatal(err)
	}
	validClose := creditIdentity{epoch: epoch, invocationID: "process", operation: "Execute", callID: 1, direction: "host_to_child", sequence: 1}
	for _, invalid := range []creditIdentity{
		{epoch: epoch, invocationID: "other", operation: "Execute", callID: 1, direction: "host_to_child", sequence: 1},
		{epoch: epoch, invocationID: "process", operation: "HTTPDo", callID: 1, direction: "host_to_child", sequence: 1},
		{epoch: epoch, invocationID: "process", operation: "Execute", callID: 2, direction: "host_to_child", sequence: 1},
		{epoch: epoch, invocationID: "process", operation: "Execute", callID: 1, direction: "child_to_host", sequence: 1},
		{epoch: epoch, invocationID: "process", operation: "Execute", callID: 1, direction: "host_to_child", sequence: 2},
	} {
		if err := call(gen.FlowControlKind_FLOW_CONTROL_CLOSE, invalid); err == nil {
			t.Fatalf("invalid CLOSE identity accepted: %+v", invalid)
		}
		if flow.closed || flow.cancel["still-live"] == nil || canceled != 1 {
			t.Fatal("invalid CLOSE mutated/revoked child state")
		}
	}
	if err := call(gen.FlowControlKind_FLOW_CONTROL_CLOSE, validClose); err != nil {
		t.Fatalf("valid CLOSE rejected: %v", err)
	}
	if !flow.closed || flow.cancel["still-live"] != nil || canceled != 2 {
		t.Fatal("valid CLOSE did not close and cancel the child state")
	}
	if err := call(gen.FlowControlKind_FLOW_CONTROL_CLOSE, validClose); err == nil {
		t.Fatal("replayed CLOSE accepted")
	}
	if !flow.closed || canceled != 2 {
		t.Fatal("replayed CLOSE mutated child state")
	}
}

type unusedControlCaller struct{}

func (unusedControlCaller) FlowControl(context.Context, *gen.FlowControlRequest, ...grpc.CallOption) (*gen.FlowControlResponse, error) {
	return nil, nil
}

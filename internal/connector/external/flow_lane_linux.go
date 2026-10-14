//go:build linux

package external

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type controlClass uint8

const (
	maxControlMessages      = 64
	maxControlBytes         = 256 << 10
	controlEndpointMessages = maxControlMessages / 2
)

const (
	controlReserve controlClass = iota
	controlAck
	controlUrgent
	controlClasses
)

// The host and child each own one endpoint lane. Half-sized endpoint budgets
// compose to the 64-message / 256-KiB process transport ceiling.
var controlSlots = [...]int{controlEndpointMessages / 2, controlEndpointMessages * 3 / 8, controlEndpointMessages / 8}

type controlLane struct{ slots [controlClasses]chan struct{} }

func newControlLane() *controlLane {
	l := &controlLane{}
	for i, n := range controlSlots {
		l.slots[i] = make(chan struct{}, n)
	}
	return l
}

func (l *controlLane) acquire(ctx context.Context, class controlClass) (func(), error) {
	if l == nil || class >= controlClasses {
		return nil, errors.New("invalid IPC control lane")
	}
	select {
	case l.slots[class] <- struct{}{}:
		return func() { <-l.slots[class] }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type flowControlCaller interface {
	FlowControl(context.Context, *gen.FlowControlRequest, ...grpc.CallOption) (*gen.FlowControlResponse, error)
}

func controlCall(ctx context.Context, lane *controlLane, caller flowControlCaller, class controlClass, req *gen.FlowControlRequest) (*gen.FlowControlResponse, error) {
	if proto.Size(req) > maxControlMessageBytes {
		return nil, errCreditLimit
	}
	release, err := lane.acquire(ctx, class)
	if err != nil {
		return nil, err
	}
	defer release()
	response, err := caller.FlowControl(ctx, req)
	if err != nil {
		return nil, err
	}
	if response == nil || proto.Size(response) > maxControlMessageBytes {
		return nil, errCreditIdentity
	}
	return response, nil
}

// ChildFlowControl is the reusable child-side half of DEC-017. A connector
// embeds it in its Connector gRPC server and uses its client for reservations.
type ChildFlowControl struct {
	mu         sync.Mutex
	epoch      uint64
	client     flowControlCaller
	lane       *controlLane
	inbound    map[creditIdentity]int
	outbound   map[creditIdentity]struct{}
	lastSeq    map[creditStream]uint64
	controlSeq map[creditStream]uint64
	pending    map[creditStream]bool
	cancel     map[string]context.CancelFunc
	changed    chan struct{}
	closed     bool
}

func NewChildFlowControl(epoch uint64, hostControl flowControlCaller) (*ChildFlowControl, error) {
	if epoch == 0 || hostControl == nil {
		return nil, errCreditIdentity
	}
	return &ChildFlowControl{epoch: epoch, client: hostControl, lane: newControlLane(), inbound: make(map[creditIdentity]int), outbound: make(map[creditIdentity]struct{}), lastSeq: make(map[creditStream]uint64), controlSeq: make(map[creditStream]uint64), pending: make(map[creditStream]bool), cancel: make(map[string]context.CancelFunc), changed: make(chan struct{})}, nil
}

// DialChildFlowControl opens the dedicated child→host control connection. The
// returned server handler is registered on the child's Connector service.
func DialChildFlowControl(ctx context.Context, hostServicesSocket string, epoch uint64) (*ChildFlowControl, *grpc.ClientConn, error) {
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", hostServicesSocket)
	}
	conn, err := grpc.NewClient("unix://"+hostServicesSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(ipc.MaxGRPCMessageBytes), grpc.MaxCallSendMsgSize(ipc.MaxGRPCMessageBytes)),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return nil, nil, err
	}
	control, err := NewChildFlowControl(epoch, gen.NewHostServicesClient(conn))
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return control, conn, nil
}

func (c *ChildFlowControl) RegisterInvocation(id string, cancel context.CancelFunc) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || id == "" || cancel == nil || c.cancel[id] != nil {
		return errCreditIdentity
	}
	c.cancel[id] = cancel
	return nil
}

func (c *ChildFlowControl) UnregisterInvocation(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cancel, id)
	for key := range c.inbound {
		if key.invocationID == id {
			delete(c.inbound, key)
		}
	}
	for key := range c.outbound {
		if key.invocationID == id {
			delete(c.outbound, key)
		}
	}
	c.notifyLocked()
}

func (c *ChildFlowControl) UnregisterCall(invocationID, operation string, callID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.inbound {
		if id.invocationID == invocationID && id.operation == operation && id.callID == callID {
			delete(c.inbound, id)
		}
	}
	for id := range c.outbound {
		if id.invocationID == invocationID && id.operation == operation && id.callID == callID {
			delete(c.outbound, id)
		}
	}
	for stream := range c.lastSeq {
		if stream.invocationID == invocationID && stream.operation == operation && stream.callID == callID {
			delete(c.lastSeq, stream)
		}
	}
	for stream := range c.controlSeq {
		if stream.invocationID == invocationID && stream.operation == operation && stream.callID == callID {
			delete(c.controlSeq, stream)
		}
	}
	c.notifyLocked()
}

func (c *ChildFlowControl) FlowControl(_ context.Context, req *gen.FlowControlRequest) (*gen.FlowControlResponse, error) {
	if req == nil || proto.Size(req) > maxControlMessageBytes || req.Identity == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid flow control message")
	}
	id, err := creditIdentityFromWire(req.Identity)
	if req.Kind == gen.FlowControlKind_FLOW_CONTROL_PROBE {
		if err != nil || req.Identity.Epoch != c.epoch || req.Identity.InvocationId != "probe" {
			return nil, status.Error(codes.PermissionDenied, "invalid flow control probe")
		}
		return &gen.FlowControlResponse{Kind: gen.FlowControlKind_FLOW_CONTROL_PROBE, Identity: req.Identity}, nil
	}
	if err != nil || id.epoch != c.epoch {
		return nil, status.Error(codes.PermissionDenied, "stale flow control identity")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch req.Kind {
	case gen.FlowControlKind_FLOW_CONTROL_GRANT:
		if id.direction != "host_to_child" || req.Frames != 1 || req.ApplicationBytes > maxCreditFrameBytes {
			return nil, status.Error(codes.InvalidArgument, "invalid flow control grant")
		}
		stream := creditStream{invocationID: id.invocationID, operation: id.operation, callID: id.callID, direction: id.direction}
		if id.sequence != c.lastSeq[stream]+1 {
			return nil, status.Error(codes.InvalidArgument, "replayed flow control grant")
		}
		c.lastSeq[stream] = id.sequence
		c.inbound[id] = int(req.ApplicationBytes)
	case gen.FlowControlKind_FLOW_CONTROL_ACK:
		if id.direction != "child_to_host" {
			return nil, status.Error(codes.InvalidArgument, "invalid flow control acknowledgement")
		}
		if _, ok := c.outbound[id]; !ok {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("unknown flow control acknowledgement: operation=%s direction=%s sequence=%d", id.operation, id.direction, id.sequence))
		}
		delete(c.outbound, id)
		c.notifyLocked()
	case gen.FlowControlKind_FLOW_CONTROL_CANCEL:
		if id.operation != "Execute" || id.callID != 1 || id.direction != "host_to_child" || req.Frames != 0 || req.ApplicationBytes != 0 || len(req.Acknowledgements) != 0 {
			return nil, status.Error(codes.InvalidArgument, "invalid flow control cancel identity")
		}
		stream := creditStream{invocationID: id.invocationID, operation: id.operation, callID: id.callID, direction: id.direction}
		if id.sequence != c.controlSeq[stream]+1 {
			return nil, status.Error(codes.InvalidArgument, "replayed flow control cancel")
		}
		cancel := c.cancel[id.invocationID]
		if cancel == nil {
			return nil, status.Error(codes.PermissionDenied, "unknown flow control invocation")
		}
		c.controlSeq[stream] = id.sequence
		cancel()
		c.revokeLocked(id.invocationID)
	case gen.FlowControlKind_FLOW_CONTROL_CLOSE:
		if id.invocationID != "process" || id.operation != "Execute" || id.callID != 1 || id.direction != "host_to_child" || req.Frames != 0 || req.ApplicationBytes != 0 || len(req.Acknowledgements) != 0 {
			return nil, status.Error(codes.InvalidArgument, "invalid flow control close")
		}
		stream := creditStream{invocationID: id.invocationID, operation: id.operation, callID: id.callID, direction: id.direction}
		if id.sequence != c.controlSeq[stream]+1 {
			return nil, status.Error(codes.InvalidArgument, "replayed flow control close")
		}
		c.controlSeq[stream] = id.sequence
		c.closed = true
		for _, cancel := range c.cancel {
			cancel()
		}
		c.inbound, c.outbound, c.cancel = make(map[creditIdentity]int), make(map[creditIdentity]struct{}), make(map[string]context.CancelFunc)
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported child control message")
	}
	return &gen.FlowControlResponse{Kind: req.Kind, Identity: req.Identity}, nil
}

func (c *ChildFlowControl) revokeLocked(invocationID string) {
	delete(c.cancel, invocationID)
	for id := range c.inbound {
		if id.invocationID == invocationID {
			delete(c.inbound, id)
		}
	}
	for id := range c.outbound {
		if id.invocationID == invocationID {
			delete(c.outbound, id)
		}
	}
	c.notifyLocked()
}

func (c *ChildFlowControl) WaitDrained(ctx context.Context, invocationID string) error {
	for {
		c.mu.Lock()
		pending := false
		for id := range c.outbound {
			if id.invocationID == invocationID {
				pending = true
				break
			}
		}
		if !pending {
			c.mu.Unlock()
			return nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (c *ChildFlowControl) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *ChildFlowControl) Reserve(ctx context.Context, id creditIdentity, applicationBytes int) error {
	if !id.valid() || id.epoch != c.epoch || id.direction != "child_to_host" || applicationBytes < 0 || applicationBytes > maxCreditFrameBytes {
		return errCreditIdentity
	}
	c.mu.Lock()
	stream := creditStream{invocationID: id.invocationID, operation: id.operation, callID: id.callID, direction: id.direction}
	if c.closed || c.cancel[id.invocationID] == nil || c.pending[stream] || id.sequence != c.lastSeq[stream]+1 {
		c.mu.Unlock()
		return errCreditReplay
	}
	c.pending[stream] = true
	c.outbound[id] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, stream)
		c.mu.Unlock()
	}()
	request := &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_RESERVE, Identity: id.wire(), Frames: 1, ApplicationBytes: uint32(applicationBytes)}
	response, err := controlCall(ctx, c.lane, c.client, controlReserve, request)
	if err != nil {
		c.mu.Lock()
		delete(c.outbound, id)
		c.mu.Unlock()
		return err
	}
	if response.Kind != gen.FlowControlKind_FLOW_CONTROL_GRANT || response.Frames != 1 || response.ApplicationBytes != uint32(applicationBytes) {
		c.mu.Lock()
		delete(c.outbound, id)
		c.mu.Unlock()
		return errCreditIdentity
	}
	if response.Identity == nil || !proto.Equal(response.Identity, id.wire()) {
		c.mu.Lock()
		delete(c.outbound, id)
		c.mu.Unlock()
		return errCreditIdentity
	}
	c.mu.Lock()
	c.lastSeq[stream] = id.sequence
	c.mu.Unlock()
	return nil
}

func (c *ChildFlowControl) ReserveNext(ctx context.Context, invocationID, operation string, callID uint64, applicationBytes int) (creditIdentity, error) {
	stream := creditStream{invocationID: invocationID, operation: operation, callID: callID, direction: "child_to_host"}
	c.mu.Lock()
	sequence := c.lastSeq[stream] + 1
	c.mu.Unlock()
	id := creditIdentity{epoch: c.epoch, invocationID: invocationID, operation: operation, callID: callID, direction: "child_to_host", sequence: sequence}
	if err := c.Reserve(ctx, id, applicationBytes); err != nil {
		return creditIdentity{}, err
	}
	return id, nil
}

func (c *ChildFlowControl) ValidateIncoming(id creditIdentity, applicationBytes int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id.direction != "host_to_child" || id.epoch != c.epoch || applicationBytes < 0 {
		return errCreditIdentity
	}
	reserved, ok := c.inbound[id]
	if !ok || applicationBytes < 0 || reserved != applicationBytes || applicationBytes > maxCreditFrameBytes {
		return errCreditReplay
	}
	c.inbound[id] = -1
	return nil
}

func (c *ChildFlowControl) AckIncoming(ctx context.Context, id creditIdentity) error {
	c.mu.Lock()
	valid, exists := c.inbound[id]
	c.mu.Unlock()
	if !exists || valid != -1 || id.direction != "host_to_child" {
		return errCreditReplay
	}
	request := &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_ACK, Acknowledgements: []*gen.FlowIdentity{id.wire()}}
	response, err := controlCall(ctx, c.lane, c.client, controlAck, request)
	if err != nil {
		return err
	}
	if response.Kind != gen.FlowControlKind_FLOW_CONTROL_ACK {
		return errCreditIdentity
	}
	c.mu.Lock()
	delete(c.inbound, id)
	c.mu.Unlock()
	return nil
}

func (c *ChildFlowControl) CancelCall(invocationID, operation string, callID uint64) error {
	stream := creditStream{invocationID: invocationID, operation: operation, callID: callID, direction: "child_to_host"}
	c.mu.Lock()
	if c.closed || operation != "HTTPDo" {
		c.mu.Unlock()
		return errCreditIdentity
	}
	hasCredits := false
	for id := range c.inbound {
		if id.invocationID == invocationID && id.operation == operation && id.callID == callID {
			hasCredits = true
			break
		}
	}
	if !hasCredits {
		for id := range c.outbound {
			if id.invocationID == invocationID && id.operation == operation && id.callID == callID {
				hasCredits = true
				break
			}
		}
	}
	if !hasCredits {
		c.mu.Unlock()
		c.UnregisterCall(invocationID, operation, callID)
		return nil
	}
	sequence := c.controlSeq[stream] + 1
	c.mu.Unlock()
	id := creditIdentity{epoch: c.epoch, invocationID: invocationID, operation: operation, callID: callID, direction: "child_to_host", sequence: sequence}
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	response, err := controlCall(ctx, c.lane, c.client, controlUrgent, &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_CANCEL, Identity: id.wire()})
	if err != nil {
		return err
	}
	if response.Kind != gen.FlowControlKind_FLOW_CONTROL_CANCEL || response.Identity == nil || !proto.Equal(response.Identity, id.wire()) {
		return errCreditIdentity
	}
	c.mu.Lock()
	c.controlSeq[stream] = sequence
	c.mu.Unlock()
	c.UnregisterCall(invocationID, operation, callID)
	return nil
}

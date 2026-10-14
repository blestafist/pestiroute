//go:build linux

package external

import (
	"context"
	"errors"

	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	maxControlMessageBytes = 4 << 10
	maxCoalescedAcks       = 32
)

func (s *hostServices) FlowControl(ctx context.Context, req *gen.FlowControlRequest) (*gen.FlowControlResponse, error) {
	if req == nil || proto.Size(req) > maxControlMessageBytes {
		return nil, status.Error(codes.ResourceExhausted, "control message too large")
	}
	class := controlReserve
	if req.Kind == gen.FlowControlKind_FLOW_CONTROL_ACK {
		class = controlAck
	} else if req.Kind == gen.FlowControlKind_FLOW_CONTROL_CANCEL || req.Kind == gen.FlowControlKind_FLOW_CONTROL_CLOSE {
		class = controlUrgent
	}
	release, err := s.registry.control.acquire(ctx, class)
	if err != nil {
		return nil, status.Error(codes.ResourceExhausted, "control lane full")
	}
	defer release()
	switch req.Kind {
	case gen.FlowControlKind_FLOW_CONTROL_RESERVE:
		id, err := creditIdentityFromWire(req.Identity)
		if err != nil || id.direction != "child_to_host" || req.Frames != 1 || req.ApplicationBytes > maxCreditFrameBytes || len(req.Acknowledgements) != 0 {
			return nil, status.Error(codes.InvalidArgument, "credit reserve denied")
		}
		if err := s.registry.credits.reserve(ctx, id, int(req.ApplicationBytes)); err != nil {
			return nil, status.Error(codes.ResourceExhausted, "credit reserve unavailable")
		}
		return &gen.FlowControlResponse{Kind: gen.FlowControlKind_FLOW_CONTROL_GRANT, Identity: id.wire(), Frames: 1, ApplicationBytes: req.ApplicationBytes}, nil
	case gen.FlowControlKind_FLOW_CONTROL_ACK:
		if req.Identity != nil || len(req.Acknowledgements) == 0 || len(req.Acknowledgements) > maxCoalescedAcks || req.Frames != 0 || req.ApplicationBytes != 0 {
			return nil, status.Error(codes.InvalidArgument, "credit acknowledgement denied")
		}
		ids := make([]creditIdentity, 0, len(req.Acknowledgements))
		for _, wire := range req.Acknowledgements {
			id, err := creditIdentityFromWire(wire)
			if err != nil || id.direction != "host_to_child" {
				return nil, status.Error(codes.InvalidArgument, "credit acknowledgement denied")
			}
			ids = append(ids, id)
		}
		if err := s.registry.credits.ack(ids); err != nil {
			return nil, status.Error(codes.InvalidArgument, "credit acknowledgement denied")
		}
		return &gen.FlowControlResponse{Kind: gen.FlowControlKind_FLOW_CONTROL_ACK}, nil
	case gen.FlowControlKind_FLOW_CONTROL_CANCEL:
		id, err := creditIdentityFromWire(req.Identity)
		if err != nil || id.direction != "child_to_host" || id.operation != "HTTPDo" || req.Frames != 0 || req.ApplicationBytes != 0 || len(req.Acknowledgements) != 0 {
			return nil, status.Error(codes.InvalidArgument, "flow-control cancellation denied")
		}
		if err := s.registry.cancelHTTPCall(id); err != nil {
			if errors.Is(err, errCallbackReplay) {
				return nil, status.Error(codes.FailedPrecondition, "flow-control cancellation replay")
			}
			return nil, status.Error(codes.PermissionDenied, "flow-control cancellation denied")
		}
		return &gen.FlowControlResponse{Kind: gen.FlowControlKind_FLOW_CONTROL_CANCEL, Identity: req.Identity}, nil
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported host control message")
	}
}

func (c *Connector) grantChild(ctx context.Context, id creditIdentity, applicationBytes int) error {
	if err := c.invocations.credits.reserve(ctx, id, applicationBytes); err != nil {
		return err
	}
	request := &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_GRANT, Identity: id.wire(), Frames: 1, ApplicationBytes: uint32(applicationBytes)}
	response, err := controlCall(ctx, c.invocations.control, c.control, controlReserve, request)
	if err != nil || response.Kind != gen.FlowControlKind_FLOW_CONTROL_GRANT || response.Identity == nil || !proto.Equal(response.Identity, id.wire()) {
		c.invocations.credits.abandon(id)
		if err != nil {
			return err
		}
		return errCreditIdentity
	}
	return nil
}

func (c *Connector) ackChild(ctx context.Context, id creditIdentity) error {
	if err := c.invocations.credits.ack([]creditIdentity{id}); err != nil {
		return err
	}
	request := &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_ACK, Identity: id.wire()}
	bounded, cancel := boundedContext(ctx, queryTimeout)
	defer cancel()
	_, err := controlCall(bounded, c.invocations.control, c.control, controlAck, request)
	return err
}

func (c *Connector) acceptExecuteData(frame *gen.ExecuteFrame, applicationBytes int) (creditIdentity, error) {
	if frame == nil || frame.DataSequence == 0 {
		return creditIdentity{}, errCreditIdentity
	}
	id := creditIdentity{epoch: frame.Epoch, invocationID: frame.InvocationId, operation: "Execute", callID: frame.CallId, direction: "child_to_host", sequence: frame.DataSequence}
	if err := c.invocations.credits.delivered(id, applicationBytes); err != nil {
		return creditIdentity{}, err
	}
	return id, nil
}

func (c *Connector) cancelChild(ctx context.Context, invocationID string) error {
	id := creditIdentity{epoch: c.epoch, invocationID: invocationID, operation: "Execute", callID: 1, direction: "host_to_child", sequence: 1}
	c.invocations.credits.revoke(invocationID)
	request := &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_CANCEL, Identity: id.wire()}
	_, err := controlCall(ctx, c.invocations.control, c.control, controlUrgent, request)
	return err
}

func (c *Connector) sendExecuteData(ctx context.Context, invocationID string, callID uint64, sequence *uint64, applicationBytes int, payload *gen.ExecuteFrame, send func(*gen.ExecuteFrame) error) error {
	*sequence++
	id := creditIdentity{epoch: c.epoch, invocationID: invocationID, operation: "Execute", callID: callID, direction: "host_to_child", sequence: *sequence}
	if err := c.grantChild(ctx, id, applicationBytes); err != nil {
		return err
	}
	payload.Epoch, payload.CallId, payload.InvocationId, payload.DataSequence = c.epoch, callID, invocationID, *sequence
	return send(payload)
}

func (s *hostServices) grantChild(ctx context.Context, id creditIdentity, applicationBytes int) error {
	if s.control == nil {
		return nil
	}
	if err := s.registry.credits.reserve(ctx, id, applicationBytes); err != nil {
		return err
	}
	request := &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_GRANT, Identity: id.wire(), Frames: 1, ApplicationBytes: uint32(applicationBytes)}
	response, err := controlCall(ctx, s.registry.control, s.control, controlReserve, request)
	if err != nil || response.Kind != gen.FlowControlKind_FLOW_CONTROL_GRANT || response.Identity == nil || !proto.Equal(response.Identity, id.wire()) {
		s.registry.credits.abandon(id)
		if err != nil {
			return err
		}
		return errCreditIdentity
	}
	return nil
}

func (s *hostServices) ackChild(ctx context.Context, id creditIdentity) error {
	if err := s.registry.credits.ack([]creditIdentity{id}); err != nil {
		return err
	}
	if s.control == nil {
		return nil
	}
	request := &gen.FlowControlRequest{Kind: gen.FlowControlKind_FLOW_CONTROL_ACK, Identity: id.wire()}
	_, err := controlCall(ctx, s.registry.control, s.control, controlAck, request)
	return err
}

func (s *hostServices) sendHTTPData(ctx context.Context, stream grpc.BidiStreamingServer[gen.HTTPFrame, gen.HTTPFrame], request *gen.HTTPFrame, sequence *uint64, applicationBytes int, frame *gen.HTTPFrame) error {
	if s.control != nil {
		*sequence++
		id := creditIdentity{epoch: request.Epoch, invocationID: request.InvocationId, operation: "HTTPDo", callID: request.CallId, direction: "host_to_child", sequence: *sequence}
		if err := s.grantChild(ctx, id, applicationBytes); err != nil {
			return err
		}
		frame.DataSequence = *sequence
	}
	frame.Epoch, frame.CallId, frame.InvocationId = request.Epoch, request.CallId, request.InvocationId
	return stream.Send(frame)
}

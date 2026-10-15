//go:build linux

package external

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Execute forwards one already-admitted attempt. Request and response bodies
// stay in bounded chunks; no retry is inferred from a transport failure.
func (c *Connector) Execute(ctx context.Context, req core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	callerCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	if err := c.acquire(ctx); err != nil {
		cancel()
		if errors.Is(err, errConnectorClosing) {
			return core.ExecutionResponse{}, &core.GatewayError{Code: "connector_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "External connector is closing or unavailable"}
		}
		return core.ExecutionResponse{}, executeGatewayError(err, false, callerCtx)
	}
	c.mu.Lock()
	initialized := c.initialized
	valid := !c.closing && !c.unavailable && req.Payload.Protocol != "" && scope.ID != "" && scope.AccountID != "" && validMode(scope.Mode)
	closing := c.closing || c.unavailable
	c.mu.Unlock()
	if closing || !initialized {
		cancel()
		c.release()
		return core.ExecutionResponse{}, &core.GatewayError{Code: "connector_unavailable", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "External connector is unavailable (not initialized or closing)"}
	}
	if !valid {
		cancel()
		c.release()
		return core.ExecutionResponse{}, &core.GatewayError{Code: "invalid_request", Category: core.CategoryInvalidRequest, RetryDisposition: core.RetryUnsafe, Message: "Invalid or unavailable execution request"}
	}
	if scope.MaxBodyBytes <= 0 {
		scope.MaxBodyBytes = int64(^uint64(0) >> 1)
	}
	if int64(len(req.Payload.Body)) > scope.MaxBodyBytes {
		cancel()
		c.release()
		return core.ExecutionResponse{}, &core.GatewayError{Code: "invalid_request", Category: core.CategoryInvalidRequest, RetryDisposition: core.RetryUnsafe, Message: "Execution request body exceeds the admitted route limit"}
	}
	invocationID, invokeCtx, err := c.invocations.beginWithLogger(ctx, scope.AccountID, services.Credentials, services.Transport, services.Logger)
	if err != nil {
		cancel()
		c.release()
		return core.ExecutionResponse{}, executeGatewayError(err, false, callerCtx)
	}
	var once sync.Once
	cleanup := func() { once.Do(func() { cancel(); c.invocations.revoke(c.epoch, invocationID); c.release() }) }
	stream, err := c.client.Execute(invokeCtx)
	var dataSequence uint64
	requestSendAttempted := false
	send := func(frame *gen.ExecuteFrame) error {
		requestSendAttempted = true
		return stream.Send(frame)
	}
	if err == nil {
		start := &gen.ExecuteStart{Request: nil, Scope: ipc.AttemptTo(scope), MaxBodyBytes: scope.MaxBodyBytes}
		start.Request, err = ipc.ExecutionRequestTo(req)
		if err == nil {
			id := ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 0, Type: "ExecuteStart"}
			err = ipc.WriteRecord(id, start, func(fragment *gen.RecordFragment) error {
				fragment.InvocationId = invocationID
				return c.sendExecuteData(invokeCtx, invocationID, 1, &dataSequence, len(fragment.Data), &gen.ExecuteFrame{Payload: &gen.ExecuteFrame_Record{Record: fragment}}, send)
			})
		}
	}
	if err == nil {
		err = ipc.WriteBody(req.Payload.Body, uint64(scope.MaxBodyBytes), func(body []byte) error {
			chunk, e := ipc.BodyChunkTo(body)
			if e != nil {
				return e
			}
			return c.sendExecuteData(invokeCtx, invocationID, 1, &dataSequence, len(body), &gen.ExecuteFrame{Payload: &gen.ExecuteFrame_Body{Body: chunk}}, send)
		})
	}
	if err == nil {
		err = ipc.WriteRecord(ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 1, Type: "InputComplete"}, &gen.InputComplete{}, func(fragment *gen.RecordFragment) error {
			fragment.InvocationId = invocationID
			return c.sendExecuteData(invokeCtx, invocationID, 1, &dataSequence, len(fragment.Data), &gen.ExecuteFrame{Payload: &gen.ExecuteFrame_Record{Record: fragment}}, send)
		})
	}
	if err == nil {
		err = stream.CloseSend()
	}
	if err != nil {
		c.signalExecuteFatal(err, callerCtx)
		cleanup()
		return core.ExecutionResponse{}, executeGatewayError(err, requestSendAttempted, callerCtx)
	}
	response, gatewayErr := readExecuteResponse(c, stream, c.epoch, invocationID, callerCtx, cleanup)
	if gatewayErr != nil {
		cleanup()
		return core.ExecutionResponse{}, gatewayErr
	}
	return response, nil
}

func (c *Connector) signalExecuteFatal(err error, ctx context.Context) {
	if err == nil || ctx != nil && ctx.Err() != nil {
		return
	}
	if errors.Is(err, ipc.ErrMalformed) || errors.Is(err, errCreditIdentity) || errors.Is(err, errCreditReplay) {
		c.signalFatal()
		return
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.Unknown, codes.Internal, codes.DataLoss:
		c.signalFatal()
	}
}

func executeGatewayError(err error, deliveredOrAmbiguous bool, callerCtx context.Context) *core.GatewayError {
	if callerCtx != nil && errors.Is(callerCtx.Err(), context.Canceled) {
		return &core.GatewayError{Code: "execute_cancelled", Category: core.CategoryCancelled, RetryDisposition: core.RetryUnknown, Message: "External connector execution was cancelled"}
	}
	if callerCtx != nil && errors.Is(callerCtx.Err(), context.DeadlineExceeded) {
		return &core.GatewayError{Code: "connector_timeout", Category: core.CategoryTimeout, RetryDisposition: core.RetryUnknown, Message: "External connector execution timed out"}
	}
	if callerCtx != nil && status.Code(err) == codes.DeadlineExceeded {
		if deadline, ok := callerCtx.Deadline(); ok && !time.Now().Before(deadline) {
			return &core.GatewayError{Code: "connector_timeout", Category: core.CategoryTimeout, RetryDisposition: core.RetryUnknown, Message: "External connector execution timed out"}
		}
	}
	if errors.Is(err, ipc.ErrMalformed) || errors.Is(err, errCreditIdentity) || errors.Is(err, errCreditReplay) {
		return &core.GatewayError{Code: "connector_protocol_error", Category: core.CategoryInvalidRequest, RetryDisposition: core.RetryUnsafe, Message: "External connector returned a malformed execution stream"}
	}
	retry := core.RetrySafe
	if deliveredOrAmbiguous {
		retry = core.RetryUnknown
	}
	return &core.GatewayError{Code: "connector_unavailable", Category: core.CategoryUnavailable, RetryDisposition: retry, Message: "External connector execution failed"}
}

func executeFrameIdentity(frame *gen.ExecuteFrame, epoch uint64, invocationID string) error {
	if frame == nil || frame.Epoch != epoch || frame.CallId != 1 || frame.InvocationId != invocationID {
		return ipc.ErrMalformed
	}
	return nil
}

func readExecuteResponse(c *Connector, stream grpc.BidiStreamingClient[gen.ExecuteFrame, gen.ExecuteFrame], epoch uint64, invocationID string, ctx context.Context, cleanup func()) (core.ExecutionResponse, *core.GatewayError) {
	first, err := stream.Recv()
	if err != nil {
		c.signalExecuteFatal(err, ctx)
		return core.ExecutionResponse{}, executeGatewayError(err, true, ctx)
	}
	if err = executeFrameIdentity(first, epoch, invocationID); err != nil {
		c.signalExecuteFatal(err, ctx)
		return core.ExecutionResponse{}, executeGatewayError(err, true, ctx)
	}
	if record, ok := first.Payload.(*gen.ExecuteFrame_Record); ok {
		if record.Record == nil || record.Record.RecordType != "HeadFrame" && record.Record.RecordType != "GatewayError" {
			c.signalExecuteFatal(ipc.ErrMalformed, ctx)
			return core.ExecutionResponse{}, executeGatewayError(ipc.ErrMalformed, true, ctx)
		}
		var wire gen.HeadFrame
		if record.Record.RecordType == "GatewayError" {
			var gateway gen.GatewayError
			if err := readExecuteRecord(c, stream, epoch, invocationID, ctx, 0, "GatewayError", first.DataSequence, record.Record, &gateway); err != nil {
				c.signalExecuteFatal(err, ctx)
				return core.ExecutionResponse{}, executeGatewayError(err, true, ctx)
			}
			ge, err := ipc.GatewayErrorFrom(&gateway)
			if err != nil || ge == nil {
				c.signalExecuteFatal(ipc.ErrMalformed, ctx)
				return core.ExecutionResponse{}, executeGatewayError(ipc.ErrMalformed, true, ctx)
			}
			if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
				c.signalExecuteFatal(ipc.ErrMalformed, ctx)
				return core.ExecutionResponse{}, executeGatewayError(ipc.ErrMalformed, true, ctx)
			}
			return core.ExecutionResponse{}, ge
		}
		if err := readExecuteRecord(c, stream, epoch, invocationID, ctx, 0, "HeadFrame", first.DataSequence, record.Record, &wire); err != nil {
			c.signalExecuteFatal(err, ctx)
			return core.ExecutionResponse{}, executeGatewayError(err, true, ctx)
		}
		head, err := ipc.HeadFrom(&wire)
		if err != nil {
			c.signalExecuteFatal(err, ctx)
			return core.ExecutionResponse{}, executeGatewayError(err, true, ctx)
		}
		closeCtx, closeCancel := context.WithCancel(context.Background())
		responseStream := &executeResponseStream{connector: c, stream: stream, epoch: epoch, invocationID: invocationID, head: &head, ctx: ctx, closeCtx: closeCtx, closeCancel: closeCancel, cleanup: cleanup}
		responseStream.stopParent = context.AfterFunc(ctx, func() { _ = responseStream.Close() })
		return core.ExecutionResponse{Stream: responseStream}, nil
	}
	c.signalExecuteFatal(ipc.ErrMalformed, ctx)
	return core.ExecutionResponse{}, executeGatewayError(ipc.ErrMalformed, true, ctx)
}

// readExecuteRecord consumes the rest of the first logical record; response
// records are never interleaved, as required by the IPC binding.
func readExecuteRecord(c *Connector, stream grpc.BidiStreamingClient[gen.ExecuteFrame, gen.ExecuteFrame], epoch uint64, invocationID string, ctx context.Context, recordID uint64, typ string, initialSequence uint64, initial *gen.RecordFragment, dst protoreflect.ProtoMessage) error {
	if initial == nil || initial.InvocationId != invocationID || initial.RecordId != recordID {
		return ipc.ErrMalformed
	}
	a := ipc.NewAssembler(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: recordID, Type: typ})
	id, err := c.acceptExecuteData(&gen.ExecuteFrame{Epoch: epoch, CallId: 1, InvocationId: invocationID, DataSequence: initialSequence, Payload: &gen.ExecuteFrame_Record{Record: initial}}, len(initial.Data))
	if err != nil {
		return err
	}
	if err := a.Add(initial); err != nil {
		return err
	}
	if err := c.ackChild(ctx, id); err != nil {
		return err
	}
	for !a.Complete() {
		frame, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := executeFrameIdentity(frame, epoch, invocationID); err != nil {
			return err
		}
		record, ok := frame.Payload.(*gen.ExecuteFrame_Record)
		if !ok || record.Record == nil {
			return ipc.ErrMalformed
		}
		if record.Record.InvocationId != invocationID {
			return ipc.ErrMalformed
		}
		id, err := c.acceptExecuteData(frame, len(record.Record.Data))
		if err != nil {
			return err
		}
		if err := a.Add(record.Record); err != nil {
			return err
		}
		if err := c.ackChild(ctx, id); err != nil {
			return err
		}
	}
	return a.Decode(dst)
}

type executeResponseStream struct {
	connector    *Connector
	stream       grpc.BidiStreamingClient[gen.ExecuteFrame, gen.ExecuteFrame]
	epoch        uint64
	invocationID string
	head         *core.HeadFrame
	ctx          context.Context
	stateMu      sync.Mutex
	done         bool
	closeCtx     context.Context
	closeCancel  context.CancelFunc
	closeOnce    sync.Once
	stopParent   func() bool
	cleanup      func()
}

func (s *executeResponseStream) Next(ctx context.Context) (core.StreamFrame, error) {
	s.stateMu.Lock()
	if s.done {
		s.stateMu.Unlock()
		return core.StreamFrame{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		s.stateMu.Unlock()
		s.Close()
		return core.StreamFrame{}, err
	}
	if err := s.ctx.Err(); err != nil {
		s.stateMu.Unlock()
		s.Close()
		return core.StreamFrame{}, err
	}
	if s.head != nil {
		h := s.head
		s.head = nil
		s.stateMu.Unlock()
		return core.StreamFrame{Type: core.FrameHead, Head: h}, nil
	}
	s.stateMu.Unlock()
	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			s.Close()
		case <-watchStop:
			if ctx.Err() != nil {
				s.Close()
			}
		}
	}()
	defer func() {
		close(watchStop)
		<-watchDone
	}()
	for {
		frame, err := s.stream.Recv()
		if ctx.Err() != nil {
			s.Close()
			return core.StreamFrame{}, ctx.Err()
		}
		if parentErr := s.ctx.Err(); parentErr != nil {
			s.Close()
			return core.StreamFrame{}, parentErr
		}
		if err != nil {
			return s.fail(err)
		}
		if err := executeFrameIdentity(frame, s.epoch, s.invocationID); err != nil {
			return s.fail(err)
		}
		switch p := frame.Payload.(type) {
		case *gen.ExecuteFrame_Body:
			body, err := ipc.BodyChunkFrom(p.Body)
			if err != nil {
				return s.fail(err)
			}
			if ctx.Err() != nil {
				s.Close()
				return core.StreamFrame{}, ctx.Err()
			}
			id, err := s.connector.acceptExecuteData(frame, len(body))
			if err != nil {
				return s.fail(err)
			}
			ackCtx, cancelAck := context.WithCancel(ctx)
			stopStreamCancel := context.AfterFunc(s.ctx, cancelAck)
			stopCloseCancel := context.AfterFunc(s.closeCtx, cancelAck)
			err = s.connector.ackChild(ackCtx, id)
			stopStreamCancel()
			stopCloseCancel()
			cancelAck()
			if err != nil {
				if ctx.Err() != nil || s.ctx.Err() != nil || s.isClosed() {
					s.Close()
					if ctx.Err() != nil {
						return core.StreamFrame{}, ctx.Err()
					}
					return core.StreamFrame{}, context.Canceled
				}
				return s.fail(err)
			}
			return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: body}}, nil
		case *gen.ExecuteFrame_Record:
			if p.Record == nil || p.Record.RecordType != "CompleteFrame" {
				return s.fail(ipc.ErrMalformed)
			}
			var wire gen.CompleteFrame
			if err := readExecuteRecord(s.connector, s.stream, s.epoch, s.invocationID, ctx, 2, "CompleteFrame", frame.DataSequence, p.Record, &wire); err != nil {
				if ctx.Err() != nil {
					s.Close()
					return core.StreamFrame{}, ctx.Err()
				}
				return s.fail(err)
			}
			complete, err := ipc.CompleteFrom(&wire)
			if err != nil {
				return s.fail(err)
			}
			if _, err := s.stream.Recv(); !errors.Is(err, io.EOF) {
				if ctx.Err() != nil {
					s.Close()
					return core.StreamFrame{}, ctx.Err()
				}
				if err == nil {
					return s.fail(ipc.ErrMalformed)
				}
				return s.fail(err)
			}
			if ctx.Err() != nil {
				s.Close()
				return core.StreamFrame{}, ctx.Err()
			}
			s.stateMu.Lock()
			s.done = true
			s.closeOnce.Do(s.closeCancel)
			stopParent := s.stopParent
			s.stopParent = nil
			s.stateMu.Unlock()
			if stopParent != nil {
				stopParent()
			}
			s.cleanup()
			return core.StreamFrame{Type: core.FrameComplete, Complete: &complete}, nil
		default:
			return s.fail(ipc.ErrMalformed)
		}
	}
}

func (s *executeResponseStream) fail(err error) (core.StreamFrame, error) {
	if parentErr := s.ctx.Err(); parentErr != nil {
		s.Close()
		return core.StreamFrame{}, parentErr
	}
	// gRPC may surface the deadline status just before its parent Context has
	// propagated cancellation to this stream wrapper.
	if status.Code(err) == codes.DeadlineExceeded {
		if deadline, ok := s.ctx.Deadline(); ok && !time.Now().Before(deadline) {
			s.Close()
			return core.StreamFrame{}, context.DeadlineExceeded
		}
	}
	if errors.Is(err, io.EOF) {
		err = ipc.ErrMalformed
	}
	if !s.isClosed() {
		s.connector.signalExecuteFatal(err, s.ctx)
	}
	s.Close()
	return core.StreamFrame{}, executeGatewayError(err, true, s.ctx)
}

func (s *executeResponseStream) Close() error {
	s.stateMu.Lock()
	closeStream := !s.done
	s.done = true
	s.closeOnce.Do(s.closeCancel)
	stopParent := s.stopParent
	s.stopParent = nil
	s.stateMu.Unlock()
	if closeStream {
		if stopParent != nil {
			stopParent()
		}
		s.cleanup()
		ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
		_ = s.connector.cancelChild(ctx, s.invocationID)
		cancel()
	}
	return nil
}

func (s *executeResponseStream) isClosed() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.done
}

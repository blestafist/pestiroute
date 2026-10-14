//go:build linux

package external

import (
	"context"
	"errors"
	"io"
	"sync"

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
		return core.ExecutionResponse{}, queryGatewayError(err, "execute")
	}
	c.mu.Lock()
	valid := !c.closing && !c.unavailable && c.initialized && req.Payload.Protocol != "" && scope.ID != "" && scope.AccountID != "" && validMode(scope.Mode)
	c.mu.Unlock()
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
		return core.ExecutionResponse{}, queryGatewayError(err, "execute")
	}
	var once sync.Once
	cleanup := func() { once.Do(func() { cancel(); c.invocations.revoke(c.epoch, invocationID); c.release() }) }
	stream, err := c.client.Execute(invokeCtx)
	if err == nil {
		start := &gen.ExecuteStart{Request: nil, Scope: ipc.AttemptTo(scope), MaxBodyBytes: scope.MaxBodyBytes}
		start.Request, err = ipc.ExecutionRequestTo(req)
		if err == nil {
			id := ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 0, Type: "ExecuteStart"}
			err = ipc.WriteRecord(id, start, func(fragment *gen.RecordFragment) error {
				fragment.InvocationId = invocationID
				return stream.Send(&gen.ExecuteFrame{Epoch: c.epoch, CallId: 1, InvocationId: invocationID, Payload: &gen.ExecuteFrame_Record{Record: fragment}})
			})
		}
	}
	if err == nil {
		err = ipc.WriteBody(req.Payload.Body, uint64(scope.MaxBodyBytes), func(body []byte) error {
			chunk, e := ipc.BodyChunkTo(body)
			if e != nil {
				return e
			}
			return stream.Send(&gen.ExecuteFrame{Epoch: c.epoch, CallId: 1, InvocationId: invocationID, Payload: &gen.ExecuteFrame_Body{Body: chunk}})
		})
	}
	if err == nil {
		err = ipc.WriteRecord(ipc.Identity{Epoch: c.epoch, CallID: 1, RecordID: 1, Type: "InputComplete"}, &gen.InputComplete{}, func(fragment *gen.RecordFragment) error {
			fragment.InvocationId = invocationID
			return stream.Send(&gen.ExecuteFrame{Epoch: c.epoch, CallId: 1, InvocationId: invocationID, Payload: &gen.ExecuteFrame_Record{Record: fragment}})
		})
	}
	if err == nil {
		err = stream.CloseSend()
	}
	if err != nil {
		cleanup()
		return core.ExecutionResponse{}, queryGatewayError(err, "execute")
	}
	response, gatewayErr := readExecuteResponse(stream, c.epoch, invocationID, callerCtx, cleanup)
	if gatewayErr != nil {
		cleanup()
		return core.ExecutionResponse{}, gatewayErr
	}
	return response, nil
}

func executeFrameIdentity(frame *gen.ExecuteFrame, epoch uint64, invocationID string) error {
	if frame == nil || frame.Epoch != epoch || frame.CallId != 1 || frame.InvocationId != invocationID {
		return ipc.ErrMalformed
	}
	return nil
}

func readExecuteResponse(stream grpc.BidiStreamingClient[gen.ExecuteFrame, gen.ExecuteFrame], epoch uint64, invocationID string, ctx context.Context, cleanup func()) (core.ExecutionResponse, *core.GatewayError) {
	first, err := stream.Recv()
	if err != nil {
		return core.ExecutionResponse{}, queryGatewayError(err, "execute")
	}
	if err = executeFrameIdentity(first, epoch, invocationID); err != nil {
		return core.ExecutionResponse{}, queryGatewayError(err, "execute")
	}
	if record, ok := first.Payload.(*gen.ExecuteFrame_Record); ok {
		if record.Record == nil || record.Record.RecordType != "HeadFrame" && record.Record.RecordType != "GatewayError" {
			return core.ExecutionResponse{}, queryGatewayError(ipc.ErrMalformed, "execute")
		}
		var wire gen.HeadFrame
		if record.Record.RecordType == "GatewayError" {
			var gateway gen.GatewayError
			if err := readExecuteRecord(stream, epoch, invocationID, 0, "GatewayError", record.Record, &gateway); err != nil {
				return core.ExecutionResponse{}, queryGatewayError(err, "execute")
			}
			ge, err := ipc.GatewayErrorFrom(&gateway)
			if err != nil || ge == nil {
				return core.ExecutionResponse{}, queryGatewayError(ipc.ErrMalformed, "execute")
			}
			if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
				return core.ExecutionResponse{}, queryGatewayError(ipc.ErrMalformed, "execute")
			}
			return core.ExecutionResponse{}, ge
		}
		if err := readExecuteRecord(stream, epoch, invocationID, 0, "HeadFrame", record.Record, &wire); err != nil {
			return core.ExecutionResponse{}, queryGatewayError(err, "execute")
		}
		head, err := ipc.HeadFrom(&wire)
		if err != nil {
			return core.ExecutionResponse{}, queryGatewayError(err, "execute")
		}
		responseStream := &executeResponseStream{stream: stream, epoch: epoch, invocationID: invocationID, head: &head, ctx: ctx, cleanup: cleanup}
		responseStream.stopParent = context.AfterFunc(ctx, func() { _ = responseStream.Close() })
		return core.ExecutionResponse{Stream: responseStream}, nil
	}
	return core.ExecutionResponse{}, queryGatewayError(ipc.ErrMalformed, "execute")
}

// readExecuteRecord consumes the rest of the first logical record; response
// records are never interleaved, as required by the IPC binding.
func readExecuteRecord(stream grpc.BidiStreamingClient[gen.ExecuteFrame, gen.ExecuteFrame], epoch uint64, invocationID string, recordID uint64, typ string, initial *gen.RecordFragment, dst protoreflect.ProtoMessage) error {
	if initial == nil || initial.InvocationId != invocationID || initial.RecordId != recordID {
		return ipc.ErrMalformed
	}
	a := ipc.NewAssembler(ipc.Identity{Epoch: epoch, CallID: 1, RecordID: recordID, Type: typ})
	if err := a.Add(initial); err != nil {
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
		if err := a.Add(record.Record); err != nil {
			return err
		}
	}
	return a.Decode(dst)
}

type executeResponseStream struct {
	stream       grpc.BidiStreamingClient[gen.ExecuteFrame, gen.ExecuteFrame]
	epoch        uint64
	invocationID string
	head         *core.HeadFrame
	ctx          context.Context
	stateMu      sync.Mutex
	done         bool
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
			if status.Code(err) == codes.DeadlineExceeded {
				s.Close()
				return core.StreamFrame{}, context.DeadlineExceeded
			}
			if status.Code(err) == codes.Canceled {
				s.Close()
				return core.StreamFrame{}, context.Canceled
			}
			return core.StreamFrame{}, err
		}
		if err := executeFrameIdentity(frame, s.epoch, s.invocationID); err != nil {
			return core.StreamFrame{}, err
		}
		switch p := frame.Payload.(type) {
		case *gen.ExecuteFrame_Body:
			body, err := ipc.BodyChunkFrom(p.Body)
			if err != nil {
				return core.StreamFrame{}, err
			}
			if ctx.Err() != nil {
				s.Close()
				return core.StreamFrame{}, ctx.Err()
			}
			return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: body}}, nil
		case *gen.ExecuteFrame_Record:
			if p.Record == nil || p.Record.RecordType != "CompleteFrame" {
				return core.StreamFrame{}, ipc.ErrMalformed
			}
			var wire gen.CompleteFrame
			if err := readExecuteRecord(s.stream, s.epoch, s.invocationID, 2, "CompleteFrame", p.Record, &wire); err != nil {
				if ctx.Err() != nil {
					s.Close()
					return core.StreamFrame{}, ctx.Err()
				}
				return core.StreamFrame{}, err
			}
			complete, err := ipc.CompleteFrom(&wire)
			if err != nil {
				return core.StreamFrame{}, err
			}
			if _, err := s.stream.Recv(); !errors.Is(err, io.EOF) {
				if ctx.Err() != nil {
					s.Close()
					return core.StreamFrame{}, ctx.Err()
				}
				if err == nil {
					return core.StreamFrame{}, ipc.ErrMalformed
				}
				return core.StreamFrame{}, err
			}
			if ctx.Err() != nil {
				s.Close()
				return core.StreamFrame{}, ctx.Err()
			}
			s.stateMu.Lock()
			s.done = true
			stopParent := s.stopParent
			s.stopParent = nil
			s.stateMu.Unlock()
			if stopParent != nil {
				stopParent()
			}
			s.cleanup()
			return core.StreamFrame{Type: core.FrameComplete, Complete: &complete}, nil
		default:
			return core.StreamFrame{}, ipc.ErrMalformed
		}
	}
}

func (s *executeResponseStream) Close() error {
	s.stateMu.Lock()
	closeStream := !s.done
	s.done = true
	stopParent := s.stopParent
	s.stopParent = nil
	s.stateMu.Unlock()
	if closeStream {
		if stopParent != nil {
			stopParent()
		}
		s.cleanup()
	}
	return nil
}

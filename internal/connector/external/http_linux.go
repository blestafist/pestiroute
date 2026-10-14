//go:build linux

package external

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var httpCallID atomic.Uint64

func httpFrameRecord(epoch, callID uint64, invocationID string, f *gen.RecordFragment) *gen.HTTPFrame {
	f.Epoch, f.CallId, f.InvocationId = epoch, callID, invocationID
	return &gen.HTTPFrame{Epoch: epoch, CallId: callID, InvocationId: invocationID, Payload: &gen.HTTPFrame_Record{Record: f}}
}

func (s *hostServices) HTTPDo(stream grpc.BidiStreamingServer[gen.HTTPFrame, gen.HTTPFrame]) error {
	first, err := stream.Recv()
	if err != nil || first == nil || first.InvocationId == "" || first.CallId == 0 || first.Epoch == 0 {
		return status.Error(codes.InvalidArgument, "HTTP request denied")
	}
	lease, _, parent, err := s.registry.admitCallback(first.Epoch, first.InvocationId, first.CallId)
	if err != nil {
		return status.Error(codes.PermissionDenied, "HTTP request denied")
	}
	defer lease.release()
	if lease.invocation.transport == nil {
		return status.Error(codes.PermissionDenied, "HTTP request denied")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stopStreamCancel := context.AfterFunc(stream.Context(), cancel)
	defer stopStreamCancel()
	body := &httpRequestStreamBody{ctx: ctx, stream: stream, epoch: first.Epoch, callID: first.CallId, invocationID: first.InvocationId, first: first}
	meta, err := body.readRequest()
	if err != nil {
		return status.Error(codes.InvalidArgument, "HTTP request denied")
	}
	body.start()
	req, err := ipc.HTTPRequestFrom(meta, body)
	if err != nil {
		return status.Error(codes.InvalidArgument, "HTTP request denied")
	}
	stripHopByHopHeaders(req.Header)
	req = req.WithContext(ctx)
	resp, err := lease.invocation.transport.Do(req)
	// Stop handing bytes to the HTTP transport once it has returned. The bounded
	// receive pump continues draining concurrently until the child half-closes.
	body.stop()
	if err != nil {
		_ = body.Close()
		return status.Error(codes.Unavailable, "HTTP request failed")
	}
	if resp == nil || resp.Body == nil {
		return status.Error(codes.Internal, "HTTP response failed")
	}
	responseHeaders := resp.Header.Clone()
	stripHopByHopHeaders(responseHeaders)
	var closeResponse sync.Once
	closeBody := func() { closeResponse.Do(func() { _ = resp.Body.Close() }) }
	defer closeBody()
	stopResponseClose := context.AfterFunc(ctx, closeBody)
	defer stopResponseClose()
	wid := ipc.Identity{Epoch: first.Epoch, CallID: first.CallId, RecordID: 0, Type: "HTTPResponse"}
	wireResponse := *resp
	wireResponse.Header = responseHeaders
	wire, err := ipc.HTTPResponseTo(&wireResponse)
	if err != nil {
		return status.Error(codes.Internal, "HTTP response failed")
	}
	if err := ipc.WriteRecord(wid, wire, func(f *gen.RecordFragment) error {
		if !lease.live() || ctx.Err() != nil {
			return context.Canceled
		}
		return stream.Send(httpFrameRecord(first.Epoch, first.CallId, first.InvocationId, f))
	}); err != nil {
		return status.Error(codes.Internal, "HTTP response failed")
	}
	if err := ipc.WriteBodyReader(resp.Body, func(chunk []byte) error {
		b, e := ipc.BodyChunkTo(chunk)
		if e != nil {
			return e
		}
		return stream.Send(&gen.HTTPFrame{Epoch: first.Epoch, CallId: first.CallId, InvocationId: first.InvocationId, Payload: &gen.HTTPFrame_Body{Body: b}})
	}); err != nil {
		return status.Error(codes.Unavailable, "HTTP response failed")
	}
	if err := ipc.WriteRecord(ipc.Identity{Epoch: first.Epoch, CallID: first.CallId, RecordID: 1, Type: "HTTPBodyComplete"}, &gen.HTTPBodyComplete{}, func(f *gen.RecordFragment) error {
		if !lease.live() || ctx.Err() != nil {
			return context.Canceled
		}
		return stream.Send(httpFrameRecord(first.Epoch, first.CallId, first.InvocationId, f))
	}); err != nil {
		return err
	}
	if err := body.wait(); err != nil {
		return status.Error(codes.InvalidArgument, "HTTP request body incomplete")
	}
	return nil
}

func stripHopByHopHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(name)
	}
}

type httpRequestStreamBody struct {
	ctx           context.Context
	stream        grpc.BidiStreamingServer[gen.HTTPFrame, gen.HTTPFrame]
	epoch, callID uint64
	invocationID  string
	first         *gen.HTTPFrame
	chunks        chan []byte
	done          chan struct{}
	readErr       error
	stopped       chan struct{}
	stopOnce      sync.Once
	current       []byte
}

func (b *httpRequestStreamBody) readRequest() (*gen.HTTPRequest, error) {
	var a *ipc.Assembler
	for {
		f, err := b.nextFrame()
		if err != nil {
			return nil, err
		}
		r := f.GetRecord()
		if r == nil {
			return nil, ipc.ErrMalformed
		}
		if a == nil {
			a = ipc.NewAssembler(ipc.Identity{Epoch: b.epoch, CallID: b.callID, RecordID: 0, Type: "HTTPRequest"})
		}
		if r.InvocationId != b.invocationID || a.Add(r) != nil {
			return nil, ipc.ErrMalformed
		}
		if a.Complete() {
			var req gen.HTTPRequest
			if a.Decode(&req) != nil {
				return nil, ipc.ErrMalformed
			}
			return &req, nil
		}
	}
}

func (b *httpRequestStreamBody) start() {
	b.chunks = make(chan []byte, 1)
	b.done = make(chan struct{})
	b.stopped = make(chan struct{})
	go b.receiveBody()
}

func (b *httpRequestStreamBody) stop() { b.stopOnce.Do(func() { close(b.stopped) }) }

func (b *httpRequestStreamBody) wait() error { <-b.done; return b.readErr }

func (b *httpRequestStreamBody) receiveBody() {
	defer close(b.chunks)
	complete, discard := false, false
	finish := func(err error) { b.readErr = err; close(b.done) }
	for {
		f, err := b.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) && complete {
				finish(nil)
			} else if errors.Is(err, io.EOF) {
				finish(ipc.ErrMalformed)
			} else {
				finish(err)
			}
			return
		}
		if f == nil || f.Epoch != b.epoch || f.CallId != b.callID || f.InvocationId != b.invocationID {
			finish(ipc.ErrMalformed)
			return
		}
		if isClosed(b.stopped) {
			discard = true
		}
		if chunk := f.GetBody(); chunk != nil {
			if complete {
				finish(ipc.ErrMalformed)
				return
			}
			data, e := ipc.BodyChunkFrom(chunk)
			if e != nil {
				finish(e)
				return
			}
			if discard {
				continue
			}
			select {
			case b.chunks <- data:
			case <-b.stopped:
				discard = true
			}
			continue
		}
		r := f.GetRecord()
		if r == nil || r.InvocationId != b.invocationID || complete {
			finish(ipc.ErrMalformed)
			return
		}
		a := ipc.NewAssembler(ipc.Identity{Epoch: b.epoch, CallID: b.callID, RecordID: 1, Type: "HTTPBodyComplete"})
		if a.Add(r) != nil || !a.Complete() {
			finish(ipc.ErrMalformed)
			return
		}
		var end gen.HTTPBodyComplete
		if a.Decode(&end) != nil {
			finish(ipc.ErrMalformed)
			return
		}
		complete = true
		if discard {
			continue
		}
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (b *httpRequestStreamBody) nextFrame() (*gen.HTTPFrame, error) {
	if b.first != nil {
		f := b.first
		b.first = nil
		return f, nil
	}
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	f, err := b.stream.Recv()
	if err != nil {
		return nil, err
	}
	if f == nil || f.Epoch != b.epoch || f.CallId != b.callID || f.InvocationId != b.invocationID {
		return nil, ipc.ErrMalformed
	}
	return f, nil
}

func (b *httpRequestStreamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if isClosed(b.stopped) {
		return 0, io.EOF
	}
	if len(b.current) == 0 {
		select {
		case data, ok := <-b.chunks:
			if ok {
				b.current = data
			} else {
				<-b.done
				if b.readErr != nil {
					return 0, b.readErr
				}
				return 0, io.EOF
			}
		case <-b.stopped:
			return 0, io.EOF
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	}
	n := copy(p, b.current)
	b.current = b.current[n:]
	return n, nil
}
func (b *httpRequestStreamBody) Close() error { b.stop(); return nil }

// HTTPDoerClient is scoped to a single host-issued invocation.
type HTTPDoerClient struct {
	client       gen.HostServicesClient
	epoch        uint64
	invocationID string
}

var _ core.HTTPDoer = (*HTTPDoerClient)(nil)

func NewHTTPDoerClient(client gen.HostServicesClient, epoch uint64, invocationID string) *HTTPDoerClient {
	return &HTTPDoerClient{client: client, epoch: epoch, invocationID: invocationID}
}

func (c *HTTPDoerClient) Do(req *http.Request) (*http.Response, error) {
	return c.doWithCallID(req, httpCallID.Add(1))
}

func (c *HTTPDoerClient) doWithCallID(req *http.Request, callID uint64) (*http.Response, error) {
	if c == nil || c.client == nil || c.epoch == 0 || c.invocationID == "" || req == nil {
		return nil, errors.New("HTTP request denied")
	}
	if callID == 0 {
		return nil, errors.New("HTTP request denied")
	}
	ctx := req.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	rpcCtx, cancel := context.WithCancel(ctx)
	stream, err := c.client.HTTPDo(rpcCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	id := ipc.Identity{Epoch: c.epoch, CallID: callID, RecordID: 0, Type: "HTTPRequest"}
	wire, err := ipc.HTTPRequestTo(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if err = ipc.WriteRecord(id, wire, func(f *gen.RecordFragment) error {
		return stream.Send(httpFrameRecord(c.epoch, callID, c.invocationID, f))
	}); err != nil {
		cancel()
		return nil, err
	}
	var bodyClose sync.Once
	closeRequestBody := func() {
		bodyClose.Do(func() {
			if req.Body != nil {
				_ = req.Body.Close()
			}
		})
	}
	stopUpload := make(chan struct{})
	var stopOnce sync.Once
	stopRequestUpload := func() { stopOnce.Do(func() { close(stopUpload); closeRequestBody() }) }
	uploadDone := make(chan error, 1)
	uploadSuccess := false
	uploadWaited := false
	go func() {
		var sendErr error
		if req.Body != nil && req.Body != http.NoBody {
			sendErr = ipc.WriteBodyReader(req.Body, func(chunk []byte) error {
				if isClosed(stopUpload) {
					return context.Canceled
				}
				b, e := ipc.BodyChunkTo(chunk)
				if e != nil {
					return e
				}
				return stream.Send(&gen.HTTPFrame{Epoch: c.epoch, CallId: callID, InvocationId: c.invocationID, Payload: &gen.HTTPFrame_Body{Body: b}})
			})
		}
		stopped := isClosed(stopUpload)
		if stopped && rpcCtx.Err() == nil {
			sendErr = nil
		}
		if sendErr == nil {
			sendErr = ipc.WriteRecord(ipc.Identity{Epoch: c.epoch, CallID: callID, RecordID: 1, Type: "HTTPBodyComplete"}, &gen.HTTPBodyComplete{}, func(f *gen.RecordFragment) error {
				return stream.Send(httpFrameRecord(c.epoch, callID, c.invocationID, f))
			})
		}
		closeErr := stream.CloseSend()
		if sendErr == nil {
			sendErr = closeErr
		}
		uploadDone <- sendErr
	}()
	defer func() {
		if !uploadSuccess {
			cancel()
		}
		stopRequestUpload()
		if !uploadWaited {
			<-uploadDone
		}
	}()
	stopOnContext := context.AfterFunc(ctx, stopRequestUpload)
	defer stopOnContext()
	a := ipc.NewAssembler(ipc.Identity{Epoch: c.epoch, CallID: callID, RecordID: 0, Type: "HTTPResponse"})
	for !a.Complete() {
		f, e := stream.Recv()
		if e != nil {
			cancel()
			return nil, e
		}
		if f == nil || f.Epoch != c.epoch || f.CallId != callID || f.InvocationId != c.invocationID || f.GetRecord() == nil {
			cancel()
			return nil, ipc.ErrMalformed
		}
		stopRequestUpload()
		if f.GetRecord().InvocationId != c.invocationID || a.Add(f.GetRecord()) != nil {
			cancel()
			return nil, ipc.ErrMalformed
		}
	}
	if err := <-uploadDone; err != nil {
		uploadWaited = true
		cancel()
		return nil, err
	}
	uploadWaited = true
	var meta gen.HTTPResponse
	if err = a.Decode(&meta); err != nil {
		cancel()
		return nil, err
	}
	body := &httpResponseStreamBody{stream: stream, epoch: c.epoch, callID: callID, invocationID: c.invocationID, cancel: cancel}
	resp, err := ipc.HTTPResponseFrom(&meta, body)
	if err != nil {
		cancel()
		return nil, err
	}
	uploadSuccess = true
	return resp, nil
}

type httpResponseStreamBody struct {
	stream        grpc.BidiStreamingClient[gen.HTTPFrame, gen.HTTPFrame]
	epoch, callID uint64
	invocationID  string
	current       []byte
	done          bool
	terminalErr   error
	cancel        context.CancelFunc
	cancelOnce    sync.Once
}

func (b *httpResponseStreamBody) fail(err error) (int, error) {
	b.terminalErr = err
	b.done = true
	b.cancelStream()
	return 0, err
}

func (b *httpResponseStreamBody) cancelStream() {
	b.cancelOnce.Do(func() {
		if b.cancel != nil {
			b.cancel()
		}
	})
}

func (b *httpResponseStreamBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.terminalErr != nil {
		return 0, b.terminalErr
	}
	for len(b.current) == 0 && !b.done {
		f, err := b.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return b.fail(ipc.ErrMalformed)
			}
			return b.fail(err)
		}
		if f == nil || f.Epoch != b.epoch || f.CallId != b.callID || f.InvocationId != b.invocationID {
			return b.fail(ipc.ErrMalformed)
		}
		if chunk := f.GetBody(); chunk != nil {
			b.current, err = ipc.BodyChunkFrom(chunk)
			if err != nil {
				return b.fail(err)
			}
			continue
		}
		r := f.GetRecord()
		if r == nil || r.InvocationId != b.invocationID {
			return b.fail(ipc.ErrMalformed)
		}
		a := ipc.NewAssembler(ipc.Identity{Epoch: b.epoch, CallID: b.callID, RecordID: 1, Type: "HTTPBodyComplete"})
		if a.Add(r) != nil || !a.Complete() {
			return b.fail(ipc.ErrMalformed)
		}
		var complete gen.HTTPBodyComplete
		if a.Decode(&complete) != nil {
			return b.fail(ipc.ErrMalformed)
		}
		if _, err = b.stream.Recv(); errors.Is(err, io.EOF) {
			b.done = true
			break
		} else if err != nil {
			return b.fail(err)
		} else {
			return b.fail(ipc.ErrMalformed)
		}
	}
	if len(b.current) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.current)
	b.current = b.current[n:]
	return n, nil
}
func (b *httpResponseStreamBody) Close() error { b.cancelStream(); return nil }

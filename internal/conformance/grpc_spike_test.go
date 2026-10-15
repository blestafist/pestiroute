package conformance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	grpcspike "github.com/blestafist/pestiroute/internal/conformance/grpc_spike"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	grpcExecute uint32 = iota + 1
	grpcHead
	grpcBody
	grpcCredential
	grpcCredentialReply
	grpcHTTP
	grpcHTTPReply
	grpcCredit
	grpcComplete
)

const grpcCallID uint64 = 0x4d362d3030340001
const grpcQueueLimit = ipcFrameBytes + ipcMaxHeaders + ipcEnvelopeBytes

// gRPC's message-size options count protobuf bytes; five more bytes are sent on the wire.
const grpcMessageLimit = ipcWireFrameMax - 5
const grpcJointCancelMarker = "joint-cancel"
const grpcQueueFrames = 64
const grpcQueueBodyBytes = 4 << 20
const grpcQueueWireBytes = 5*1024*1024 + 256*1024

// TestM6IPCGRPCChild is the disposable child-process server for this spike.
func TestM6IPCGRPCChild(t *testing.T) {
	if os.Getenv("PESTIROUTE_M6_GRPC_CHILD") != "1" {
		return
	}
	path := os.Getenv("PESTIROUTE_M6_GRPC_SOCKET")
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		os.Exit(41)
	}
	s := grpc.NewServer(grpc.MaxRecvMsgSize(grpcMessageLimit), grpc.MaxSendMsgSize(grpcMessageLimit), grpc.InitialWindowSize(ipcFrameBytes))
	grpcspike.RegisterBridgeServer(s, grpcScenarioServer{stop: s.GracefulStop})
	if err := s.Serve(l); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		os.Exit(42)
	}
}

type grpcScenarioServer struct {
	grpcspike.UnimplementedBridgeServer
	stop func()
}

func (grpcScenario grpcScenarioServer) Execute(stream grpc.BidiStreamingServer[grpcspike.Frame, grpcspike.Frame]) (result error) {
	defer func() {
		if result != nil {
			fmt.Fprintln(os.Stderr, "gRPC spike handler:", result)
		}
	}()
	f, err := stream.Recv()
	if err == nil && f.GetKind() == grpcExecute && f.GetCorrelation() == grpcCallID && bytes.Equal(f.GetData(), []byte(grpcJointCancelMarker)) {
		if err := grpcRunJointCancellation(stream); err != nil {
			return err
		}
		go grpcScenario.stop()
		return nil
	}
	if err == nil && f.GetKind() == grpcExecute && f.GetCorrelation() == grpcCallID {
		if handled, handleErr := grpcHandleBoundary(stream, f); handled {
			if handleErr == nil {
				go grpcScenario.stop()
			}
			return handleErr
		}
	}
	if err != nil || f.GetKind() != grpcExecute || f.GetCorrelation() != grpcCallID || !validGRPCRequest(f.GetData()) {
		return status.Error(3, "invalid Execute")
	}
	send := func(kind uint32, data []byte) error {
		return stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: kind, Data: data})
	}
	recv := func(kind uint32, data []byte) error {
		f, e := stream.Recv()
		if e != nil {
			return e
		}
		if f.GetCorrelation() != grpcCallID || f.GetKind() != kind || !bytes.Equal(f.GetData(), data) {
			return status.Error(3, "invalid reverse control")
		}
		return nil
	}
	if err := send(grpcCredential, []byte("bearer")); err != nil {
		return err
	}
	if err := recv(grpcCredentialReply, []byte("synthetic-only")); err != nil {
		return err
	}
	if err := send(grpcHead, nil); err != nil {
		return err
	}
	chunk := make([]byte, ipcFrameBytes)
	copy(chunk, []byte{'b', 'i', 'n', 'a', 'r', 'y', 0, 0xff})
	if err := send(grpcBody, chunk); err != nil {
		return err
	}
	if err := recv(grpcCredit, nil); err != nil {
		return err
	}
	if err := send(grpcBody, chunk); err != nil {
		return err
	}
	if err := send(grpcCredential, []byte("bearer")); err != nil {
		return err
	}
	if err := recv(grpcCredentialReply, []byte("synthetic-only")); err != nil {
		return err
	}
	callback := append([]byte{0, 0xff}, bytes.Repeat([]byte{'c'}, ipcCallbackSize-2)...)
	for off := 0; off < len(callback); off += ipcHTTPChunk {
		end := off + ipcHTTPChunk
		if end > len(callback) {
			end = len(callback)
		}
		part := callback[off:end]
		data := part
		if off == 0 {
			data = append(bytes.Repeat([]byte{'h'}, ipcHeaderBytes), part...)
		}
		if err := send(grpcHTTP, data); err != nil {
			return err
		}
		if err := recv(grpcHTTPReply, part); err != nil {
			return err
		}
	}
	// The second output Body stayed queued during the reverse callback; its
	// explicit credit releases that retained frame before streaming resumes.
	if err := recv(grpcCredit, nil); err != nil {
		return err
	}
	for i := 2; i < ipcFrames; i++ {
		if err := send(grpcBody, chunk); err != nil {
			return err
		}
		if i+1 < ipcFrames {
			if err := recv(grpcCredit, nil); err != nil {
				return err
			}
		}
	}
	if err := send(grpcComplete, nil); err != nil {
		return err
	}
	// Complete is not process completion: await the client's explicit half-close.
	if extra, err := stream.Recv(); err != io.EOF {
		return fmt.Errorf("after Complete, client frame=%+v err=%v, want EOF", extra, err)
	}
	go grpcScenario.stop()
	return nil
}

func grpcRunJointCancellation(stream grpc.BidiStreamingServer[grpcspike.Frame, grpcspike.Frame]) error {
	callbackData := append(bytes.Repeat([]byte{'h'}, ipcHeaderBytes), bytes.Repeat([]byte{'c'}, ipcCallbackSize)...)
	callback := &grpcspike.Frame{Correlation: grpcCallID, Kind: grpcHTTP, Data: callbackData}
	if err := stream.Send(callback); err != nil {
		return err
	}
	callbackDone := make(chan error, 1)
	go func() { _, err := stream.Recv(); callbackDone <- err }()
	select {
	case err := <-callbackDone:
		return fmt.Errorf("callback receive did not block: %v", err)
	case <-time.After(50 * time.Millisecond):
		fmt.Fprintln(os.Stderr, "M6-004_CALLBACK_RECV_BLOCKED")
	}
	chunk := make([]byte, ipcFrameBytes)
	attempt := make(chan int, 1)
	sendResult := make(chan error, 1)
	outputDone := make(chan error, 1)
	go func() {
		// Reserve one of the 64 per-call frames for the reverse callback.
		for i := 0; i < grpcQueueFrames-1; i++ {
			attempt <- i
			err := stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcBody, Data: chunk})
			sendResult <- err
			if err != nil {
				return
			}
		}
		attempt <- grpcQueueFrames - 1
		outputDone <- errors.New("output Send did not block within the 64-frame bound")
	}()
	blocked := false
	for !blocked {
		index := <-attempt
		if index == grpcQueueFrames-1 {
			return <-outputDone
		}
		select {
		case err := <-sendResult:
			if err != nil {
				return err
			}
		case <-time.After(50 * time.Millisecond):
			fmt.Fprintf(os.Stderr, "M6-004_OUTPUT_SEND_BLOCKED:%d\n", index)
			blocked = true
		}
	}
	select {
	case <-stream.Context().Done():
	case <-time.After(2 * time.Second):
		return errors.New("joint cancellation did not reach child RPC within 2 seconds")
	}
	callbackErr := <-callbackDone
	outputErr := <-sendResult
	if status.Code(callbackErr) != 1 || status.Code(outputErr) != 1 {
		return fmt.Errorf("joint cancellation releases: callback=%v output=%v", callbackErr, outputErr)
	}
	return nil
}

func grpcHandleBoundary(stream grpc.BidiStreamingServer[grpcspike.Frame, grpcspike.Frame], request *grpcspike.Frame) (bool, error) {
	switch {
	case bytes.HasPrefix(request.GetData(), []byte("boundary-client")):
		if proto.Size(request) != grpcMessageLimit {
			return true, status.Error(3, "client boundary size mismatch")
		}
		if err := stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcComplete}); err != nil {
			return true, err
		}
		return true, grpcWaitHalfClose(stream)
	case bytes.Equal(request.GetData(), []byte("boundary-server")):
		return true, grpcSendBoundaryAndWait(stream, grpcMessageLimit)
	case bytes.Equal(request.GetData(), []byte("overflow-server")):
		return true, grpcSendBoundaryAndWait(stream, grpcMessageLimit+1)
	default:
		return false, nil
	}
}

func grpcSendBoundaryAndWait(stream grpc.BidiStreamingServer[grpcspike.Frame, grpcspike.Frame], limit int) error {
	data := grpcDataForMessageSize(limit, grpcBody)
	if data == nil {
		return status.Error(3, "unrepresentable protobuf boundary")
	}
	if err := stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcBody, Data: data}); err != nil {
		return err
	}
	ack, err := stream.Recv()
	if err != nil {
		return err
	}
	if ack.GetKind() != grpcCredit || ack.GetCorrelation() != grpcCallID || len(ack.GetData()) != 0 {
		return status.Error(3, "invalid boundary credit")
	}
	if err := stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcComplete}); err != nil {
		return err
	}
	return grpcWaitHalfClose(stream)
}

func grpcWaitHalfClose(stream grpc.BidiStreamingServer[grpcspike.Frame, grpcspike.Frame]) error {
	if _, err := stream.Recv(); err != io.EOF {
		return fmt.Errorf("after Complete, client half-close = %v, want EOF", err)
	}
	return nil
}

func grpcDataForMessageSize(size int, kind uint32) []byte {
	frame := &grpcspike.Frame{Correlation: grpcCallID, Kind: kind}
	for n := size - 32; n <= size; n++ {
		if n < 0 {
			continue
		}
		frame.Data = make([]byte, n)
		if proto.Size(frame) == size {
			return frame.Data
		}
	}
	return nil
}

func validGRPCRequest(b []byte) bool {
	payload := stdioRequestPayload()[ipcHeaderBytes:]
	if len(b) != ipcHeaderBytes+len(payload) {
		return false
	}
	for _, c := range b[:ipcHeaderBytes] {
		if c != 'h' {
			return false
		}
	}
	return bytes.Equal(b[ipcHeaderBytes:], payload)
}

type grpcMeasure struct {
	first, elapsed, reap, socketCleanup, cpu, rss, queued int64
	cancelNanos                                           int64
	frames                                                int
	throughput                                            float64
}

func runGRPCScenario(t *testing.T, stall <-chan struct{}, arrived chan<- struct{}) grpcMeasure {
	t.Helper()
	start := time.Now()
	dir := t.TempDir()
	sock := filepath.Join(dir, "child.sock")
	deadline := time.Now().Add(10 * time.Second)
	cmd := exec.Command(os.Args[0], "-test.run=^TestM6IPCGRPCChild$")
	cmd.Env = append(os.Environ(), "PESTIROUTE_M6_GRPC_CHILD=1", "PESTIROUTE_M6_GRPC_SOCKET="+sock, "GORACE=atexit_sleep_ms=0")
	cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	watchdog := time.AfterFunc(time.Until(deadline), func() { _ = cmd.Process.Kill() })
	finished := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(finished)
	}()
	t.Cleanup(func() {
		watchdog.Stop()
		select {
		case <-finished:
		default:
			_ = cmd.Process.Kill()
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Errorf("gRPC child not reaped")
			}
		}
		if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove owned socket during cleanup: %v", err)
		}
	})
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for {
		probe, probeErr := (&net.Dialer{}).DialContext(ctx, "unix", sock)
		if probeErr == nil {
			_ = probe.Close()
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("child Unix socket startup: %v", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}), grpc.WithInitialWindowSize(ipcFrameBytes), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(grpcMessageLimit), grpc.MaxCallSendMsgSize(grpcMessageLimit)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := grpcspike.NewBridgeClient(conn).Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sendCount := 0
	send := func(kind uint32, data []byte) {
		t.Helper()
		sendCount++
		if err := stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: kind, Data: data}); err != nil {
			select {
			case <-finished:
				t.Fatalf("send kind %d count %d: %v; child exited: %v", kind, sendCount, err, waitErr)
			default:
				t.Fatalf("send kind %d count %d: %v", kind, sendCount, err)
			}
		}
	}
	recv := func(kind uint32) []byte {
		t.Helper()
		f, e := stream.Recv()
		if e != nil {
			select {
			case <-finished:
				t.Fatalf("recv kind %d: %v (child exit %v)", kind, e, waitErr)
			default:
				t.Fatalf("recv kind %d: %v", kind, e)
			}
		}
		if f.GetKind() != kind || f.GetCorrelation() != grpcCallID {
			t.Fatalf("got kind/correlation %d/%x want %d/%x", f.GetKind(), f.GetCorrelation(), kind, grpcCallID)
		}
		if proto.Size(f)+5 > ipcWireFrameMax {
			t.Fatalf("encoded payload exceeds frame ceiling")
		}
		return f.GetData()
	}
	payload := stdioRequestPayload()[ipcHeaderBytes:]
	request := append(bytes.Repeat([]byte{'h'}, ipcHeaderBytes), payload...)
	if len(request)-ipcHeaderBytes != len(payload) {
		t.Fatal("request encoding")
	}
	send(grpcExecute, request)
	credentialCalls := 0
	first := int64(0)
	if got := recv(grpcCredential); string(got) != "bearer" {
		t.Fatalf("credential scope %q", got)
	}
	credentialCalls++
	send(grpcCredentialReply, []byte("synthetic-only"))
	if len(recv(grpcHead)) != 0 {
		t.Fatal("Head payload")
	}
	wantChunk := make([]byte, ipcFrameBytes)
	copy(wantChunk, []byte{'b', 'i', 'n', 'a', 'r', 'y', 0, 0xff})
	want := sha256.New()
	for i := 0; i < ipcFrames; i++ {
		_, _ = want.Write(wantChunk)
	}
	hash := sha256.New()
	var output int
	var queued int64
	maxQueued := int64(0)
	maxFrames := 0
	consumeBody := func(data []byte) {
		t.Helper()
		if len(data) != ipcFrameBytes || !bytes.Equal(data, wantChunk) {
			t.Fatal("body frame changed")
		}
		if first == 0 {
			first = time.Since(start).Nanoseconds()
		}
		_, _ = hash.Write(data)
		output += len(data)
	}
	consumeBody(recv(grpcBody))
	send(grpcCredit, nil)
	pendingBody := recv(grpcBody)
	if len(pendingBody) != ipcFrameBytes || !bytes.Equal(pendingBody, wantChunk) {
		t.Fatal("stalled output Body changed")
	}
	queued += int64(proto.Size(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcBody, Data: pendingBody}) + 5)
	maxQueued = queued
	maxFrames = 1
	if got := recv(grpcCredential); string(got) != "bearer" {
		t.Fatalf("reverse credential %q", got)
	}
	credentialCalls++
	send(grpcCredentialReply, []byte("synthetic-only"))
	callbackWant := append([]byte{0, 0xff}, bytes.Repeat([]byte{'c'}, ipcCallbackSize-2)...)
	for i := 0; i < ipcCallbackSize/ipcHTTPChunk; i++ {
		data := recv(grpcHTTP)
		part := data
		if i == 0 {
			if len(data) < ipcHeaderBytes || !bytes.Equal(data[:ipcHeaderBytes], bytes.Repeat([]byte{'h'}, ipcHeaderBytes)) {
				t.Fatal("HTTP callback header mismatch")
			}
			part = data[ipcHeaderBytes:]
		}
		off := i * ipcHTTPChunk
		end := off + ipcHTTPChunk
		if end > len(callbackWant) {
			end = len(callbackWant)
		}
		if !bytes.Equal(part, callbackWant[off:end]) {
			t.Fatalf("HTTP callback bytes %d", i)
		}
		queued += int64(proto.Size(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcHTTP, Data: data}) + 5)
		if queued > maxQueued {
			maxQueued = queued
		}
		if 2 > maxFrames {
			maxFrames = 2
		}
		if i == 0 && stall != nil {
			arrived <- struct{}{}
			<-stall
		}
		send(grpcHTTPReply, part)
		queued -= int64(proto.Size(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcHTTP, Data: data}) + 5)
	}
	consumeBody(pendingBody)
	queued -= int64(proto.Size(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcBody, Data: pendingBody}) + 5)
	send(grpcCredit, nil)
	for i := 2; i < ipcFrames; i++ {
		consumeBody(recv(grpcBody))
		if i+1 < ipcFrames {
			send(grpcCredit, nil)
		}
	}
	if len(recv(grpcComplete)) != 0 || output != ipcTotalBytes || credentialCalls != 2 || !bytes.Equal(hash.Sum(nil), want.Sum(nil)) {
		t.Fatal("gRPC stream/callback fidelity failure")
	}
	if maxQueued > int64(grpcQueueLimit) || maxFrames > 2 {
		t.Fatalf("queue %d bytes/%d frames exceeds M6-002 bound", maxQueued, maxFrames)
	}
	elapsed := time.Since(start).Nanoseconds()
	rss := procPeakRSS(cmd.Process.Pid)
	if rss == 0 {
		t.Fatal("child peak RSS unavailable while process is active")
	}
	_ = stream.CloseSend()
	reapStart := time.Now()
	select {
	case <-finished:
		if waitErr != nil {
			t.Fatalf("child exit: %v", waitErr)
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child reap timeout")
	}
	usage, _ := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	cpu := int64(0)
	if usage != nil {
		cpu = timeNanos(usage.Utime) + timeNanos(usage.Stime)
	}
	socketCleanupStart := time.Now()
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove owned socket: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("owned socket remains: %v", err)
	}
	socketCleanup := time.Since(socketCleanupStart).Nanoseconds()
	return grpcMeasure{first: first, elapsed: elapsed, reap: time.Since(reapStart).Nanoseconds(), socketCleanup: socketCleanup, cpu: cpu, rss: rss, queued: maxQueued, frames: maxFrames, throughput: float64(output) / (float64(elapsed) / float64(time.Second))}
}

func TestM6IPCScenarioGRPC(t *testing.T) {
	var rows []grpcMeasure
	for i := 0; i < 11; i++ {
		m := runGRPCScenario(t, nil, nil)
		if i == 0 {
			t.Logf("gRPC warmup: %+v", m)
		} else {
			rows = append(rows, m)
			t.Logf("gRPC repetition=%d first_body_ms=%.3f completion_ms=%.3f reap_ms=%.3f socket_cleanup_ms=%.3f MiB_s=%.2f cpu_ms=%.3f VmHWM_bytes=%d queue=%d/%d", i, float64(m.first)/1e6, float64(m.elapsed)/1e6, float64(m.reap)/1e6, float64(m.socketCleanup)/1e6, m.throughput/1048576, float64(m.cpu)/1e6, m.rss, m.queued, m.frames)
		}
	}
	first := make([]int64, len(rows))
	completion := make([]int64, len(rows))
	for i, m := range rows {
		first[i] = m.first
		completion[i] = m.elapsed
	}
	sortInt64(first)
	sortInt64(completion)
	t.Logf("gRPC first_body_upper_median_ms=%.3f p95_ms=%.3f completion_upper_median_ms=%.3f p95_ms=%.3f", float64(first[len(first)/2])/1e6, float64(first[9*len(first)/10])/1e6, float64(completion[len(completion)/2])/1e6, float64(completion[9*len(completion)/10])/1e6)
}

func TestM6IPCGRPCSaturationCancellation(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 4)
	cancelFourth := make(chan struct{})
	results := make(chan grpcMeasure, 4)
	for i := 0; i < 3; i++ {
		go func() { results <- runGRPCScenario(t, release, arrived) }()
	}
	go func() { results <- runGRPCJointCancellation(t, cancelFourth, arrived) }()
	for i := 0; i < 4; i++ {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("four-call saturation did not reach all held callback/output queues")
		}
	}
	close(cancelFourth)
	var cancelled grpcMeasure
	select {
	case cancelled = <-results:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("fourth call cancellation did not progress while three calls remained stalled")
	}
	if cancelled.cancelNanos <= 0 || cancelled.cancelNanos > int64(2*time.Second) {
		t.Fatalf("joint cancellation latency %v", time.Duration(cancelled.cancelNanos))
	}
	close(release)
	queued := cancelled.queued
	queuedFrames := cancelled.frames
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for i := 0; i < 3; i++ {
		select {
		case m := <-results:
			queued += m.queued
			queuedFrames += m.frames
		case <-deadline.C:
			t.Fatal("held saturation calls exceeded fixed 10-second bound")
		}
	}
	if queued > 21<<20 || queuedFrames > 256 {
		t.Fatalf("four-call aggregate retained queue = %d bytes/%d frames, exceeds M6-002 bound", queued, queuedFrames)
	}
	t.Logf("four-call peak retained encoded queue=%d bytes/%d frames; fourth joint cancel=%v; held calls resumed", queued, queuedFrames, time.Duration(cancelled.cancelNanos))
}

func TestM6IPCGRPCJointBlockedOutputAndCallbackCancellation(t *testing.T) {
	cancel := make(chan struct{})
	arrived := make(chan struct{}, 1)
	result := make(chan grpcMeasure, 1)
	go func() { result <- runGRPCJointCancellation(t, cancel, arrived) }()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("jointly blocked output and callback were not synchronized")
	}
	close(cancel)
	select {
	case m := <-result:
		if m.cancelNanos <= 0 || m.cancelNanos > int64(2*time.Second) {
			t.Fatalf("joint cancellation latency %v", time.Duration(m.cancelNanos))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("joint cancellation did not complete")
	}
}

func runGRPCJointCancellation(t *testing.T, cancelTrigger <-chan struct{}, arrived chan<- struct{}) grpcMeasure {
	t.Helper()
	p := startGRPCProbeNoKill(t)
	callbackData := append(bytes.Repeat([]byte{'h'}, ipcHeaderBytes), bytes.Repeat([]byte{'c'}, ipcCallbackSize)...)
	if err := p.stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcExecute, Data: []byte(grpcJointCancelMarker)}); err != nil {
		t.Fatal(err)
	}
	callbackSeen := make(chan struct{})
	callbackPayload := make(chan []byte, 1)
	outputQueue := make(chan []byte, 1)
	outputQueueFull := make(chan struct{})
	outputHandoffBlocked := make(chan struct{})
	readerDone := make(chan error, 1)
	go func() {
		for {
			f, err := p.stream.Recv()
			if err != nil {
				readerDone <- err
				return
			}
			switch f.GetKind() {
			case grpcHTTP:
				callbackPayload <- f.GetData()
				close(callbackSeen)
			case grpcBody:
				select {
				case outputQueue <- f.GetData():
					if len(outputQueue) == 1 {
						select {
						case <-outputQueueFull:
						default:
							close(outputQueueFull)
						}
					}
				default:
					close(outputHandoffBlocked)
					select {
					case outputQueue <- f.GetData():
					case <-p.ctx.Done():
						readerDone <- p.ctx.Err()
						return
					}
				}
			}
		}
	}()
	select {
	case <-callbackSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("reverse callback did not arrive")
	}
	callbackBlocked := make(chan struct{})
	callbackDone := make(chan error, 1)
	go func() {
		payload := <-callbackPayload
		if !bytes.Equal(payload, callbackData) {
			callbackDone <- errors.New("reverse callback payload changed")
			return
		}
		close(callbackBlocked)
		<-p.ctx.Done()
		callbackDone <- p.ctx.Err()
	}()
	select {
	case <-callbackBlocked:
	case <-time.After(2 * time.Second):
		t.Fatal("reverse callback handler did not start")
	}
	for _, wait := range []struct {
		name string
		ch   <-chan struct{}
	}{
		{name: "bounded output queue full", ch: outputQueueFull},
		{name: "output delivery blocked", ch: outputHandoffBlocked},
	} {
		select {
		case <-wait.ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s was not reached", wait.name)
		}
	}
	awaitGRPCChildEvent(t, p.events, "M6-004_CALLBACK_RECV_BLOCKED")
	outputEvent := awaitGRPCChildEventPrefix(t, p.events, "M6-004_OUTPUT_SEND_BLOCKED:")
	blockedIndex, err := strconv.Atoi(strings.TrimPrefix(outputEvent, "M6-004_OUTPUT_SEND_BLOCKED:"))
	if err != nil {
		t.Fatalf("invalid blocked output frame index %q: %v", outputEvent, err)
	}
	if len(outputQueue) != 1 {
		t.Fatalf("retained output queue length=%d, want one queued frame plus blocked handoff", len(outputQueue))
	}
	arrived <- struct{}{}
	select {
	case <-cancelTrigger:
	case <-time.After(2 * time.Second):
		t.Fatal("joint cancellation trigger exceeded 2 seconds")
	}
	started := time.Now()
	p.cancel()
	for name, done := range map[string]<-chan error{"parent callback": callbackDone, "parent output handoff": readerDone} {
		select {
		case err := <-done:
			if err == nil || !errors.Is(err, context.Canceled) {
				t.Errorf("%s returned %v, want context cancellation", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s remained blocked after cancellation", name)
		}
	}
	select {
	case <-p.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("child handler/process did not complete after both operations cancelled")
	}
	if p.waitErr != nil {
		t.Fatalf("child required forced termination instead of graceful cancellation: %v", p.waitErr)
	}
	elapsed := time.Since(started)
	if elapsed > 2*time.Second {
		t.Fatalf("joint cancellation and child completion took %v", elapsed)
	}
	if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove cancelled child socket: %v", err)
	}
	queuedBytes, queuedFrames, withinBound := grpcJointQueueOccupancy(blockedIndex, callbackData)
	if !withinBound {
		t.Fatalf("joint cancellation occupancy=%d bytes/%d frames exceeds per-call cap", queuedBytes, queuedFrames)
	}
	return grpcMeasure{cancelNanos: elapsed.Nanoseconds(), queued: queuedBytes, frames: queuedFrames}
}

func grpcJointQueueOccupancy(blockedIndex int, callbackData []byte) (int64, int, bool) {
	if blockedIndex < 2 || blockedIndex >= grpcQueueFrames-1 {
		return 0, 0, false
	}
	bodyBytes := int64(proto.Size(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcBody, Data: make([]byte, ipcFrameBytes)}) + 5)
	callbackBytes := int64(proto.Size(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcHTTP, Data: callbackData}) + 5)
	frames := blockedIndex + 2 // successful Bodies + the blocked Body + callback
	bytes := int64(blockedIndex+1)*bodyBytes + callbackBytes
	applicationBodyBytes := int64(blockedIndex+1)*ipcFrameBytes + ipcCallbackSize
	return bytes, frames, frames <= grpcQueueFrames && applicationBodyBytes <= grpcQueueBodyBytes && bytes <= grpcQueueWireBytes
}

func TestM6IPCGRPCJointQueueOccupancyBoundary(t *testing.T) {
	callbackData := append(bytes.Repeat([]byte{'h'}, ipcHeaderBytes), bytes.Repeat([]byte{'c'}, ipcCallbackSize)...)
	bytesAtLimit, framesAtLimit, ok := grpcJointQueueOccupancy(grpcQueueFrames-2, callbackData)
	if !ok || framesAtLimit != grpcQueueFrames {
		t.Fatalf("maximum retained queue = %d bytes/%d frames/ok=%v, want permitted %d-frame maximum", bytesAtLimit, framesAtLimit, ok, grpcQueueFrames)
	}
	if bodies := int64(grpcQueueFrames-1)*ipcFrameBytes + ipcCallbackSize; bodies != grpcQueueBodyBytes {
		t.Fatalf("maximum queued application bodies=%d, want exact %d-byte bound", bodies, grpcQueueBodyBytes)
	}
	bodyWireBytes := int64(proto.Size(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcBody, Data: make([]byte, ipcFrameBytes)}) + 5)
	callbackWireBytes := int64(proto.Size(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcHTTP, Data: callbackData}) + 5)
	wantEncodedBytes := int64(grpcQueueFrames-1)*bodyWireBytes + callbackWireBytes
	if bytesAtLimit != wantEncodedBytes {
		t.Fatalf("maximum encoded occupancy=%d, want %d (including the pending blocked Body)", bytesAtLimit, wantEncodedBytes)
	}
	if bytesAtLimit > grpcQueueWireBytes {
		t.Fatalf("maximum retained encoded bytes=%d, exceeds %d-byte bound", bytesAtLimit, grpcQueueWireBytes)
	}
	if _, frames, ok := grpcJointQueueOccupancy(grpcQueueFrames-1, callbackData); ok || frames != 0 {
		t.Fatalf("one-more-than-bound occupancy accepted: frames=%d ok=%v", frames, ok)
	}
}

func awaitGRPCChildEvent(t *testing.T, events <-chan string, event string) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-events:
			if line == event {
				return
			}
		case <-timer.C:
			t.Fatalf("child did not report synchronized %s", event)
		}
	}
}

func awaitGRPCChildEventPrefix(t *testing.T, events <-chan string, prefix string) string {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-events:
			if strings.HasPrefix(line, prefix) {
				return line
			}
		case <-timer.C:
			t.Fatalf("child did not report synchronized event %s", prefix)
		}
	}
}

func TestM6IPCGRPCRejectsMalformedExecuteAndStaleCorrelation(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   uint64
		kind uint32
		data []byte
	}{
		{name: "malformed frame", id: grpcCallID, kind: 99, data: []byte("short")},
		{name: "malformed payload", id: grpcCallID, kind: grpcExecute, data: []byte("short")},
		{name: "stale correlation", id: grpcCallID + 1, kind: grpcExecute, data: append(bytes.Repeat([]byte{'h'}, ipcHeaderBytes), stdioRequestPayload()[ipcHeaderBytes:]...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startGRPCProbe(t)
			if err := p.stream.Send(&grpcspike.Frame{Correlation: tc.id, Kind: tc.kind, Data: tc.data}); err != nil {
				t.Fatal(err)
			}
			_ = p.stream.CloseSend()
			if _, err := p.stream.Recv(); err == nil || status.Code(err) != 3 {
				t.Fatalf("invalid Execute response = %v, want InvalidArgument and no frames", err)
			}
		})
	}
}

func TestM6IPCGRPCMessageSizeBoundaryBothDirections(t *testing.T) {
	t.Run("client exact boundary accepted", func(t *testing.T) {
		p := startGRPCProbe(t)
		data := grpcDataForMessageSize(grpcMessageLimit, grpcExecute)
		copy(data, []byte("boundary-client"))
		if err := p.stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcExecute, Data: data}); err != nil {
			t.Fatalf("send exact client boundary: %v", err)
		}
		if f, err := p.stream.Recv(); err != nil || f.GetKind() != grpcComplete {
			t.Fatalf("exact client boundary response = %+v, %v", f, err)
		}
		finishGRPCProbe(t, p)
	})
	t.Run("client over boundary rejected", func(t *testing.T) {
		p := startGRPCProbe(t)
		data := grpcDataForMessageSize(grpcMessageLimit+1, grpcExecute)
		copy(data, []byte("boundary-client"))
		err := p.stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcExecute, Data: data})
		if err == nil || status.Code(err) != 8 {
			t.Fatalf("oversized client Send error = %v, want ResourceExhausted", err)
		}
	})
	t.Run("server exact boundary accepted", func(t *testing.T) {
		p := startGRPCProbe(t)
		if err := p.stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcExecute, Data: []byte("boundary-server")}); err != nil {
			t.Fatal(err)
		}
		f, err := p.stream.Recv()
		if err != nil || f.GetKind() != grpcBody || proto.Size(f) != grpcMessageLimit || proto.Size(f)+5 != ipcWireFrameMax {
			t.Fatalf("exact server boundary size=%d frame=%d err=%v", proto.Size(f), proto.Size(f)+5, err)
		}
		if err := p.stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcCredit}); err != nil {
			t.Fatal(err)
		}
		if f, err := p.stream.Recv(); err != nil || f.GetKind() != grpcComplete {
			t.Fatalf("server boundary completion = %+v, %v", f, err)
		}
		finishGRPCProbe(t, p)
	})
	t.Run("server over boundary rejected", func(t *testing.T) {
		p := startGRPCProbe(t)
		if err := p.stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcExecute, Data: []byte("overflow-server")}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.stream.Recv(); err == nil || status.Code(err) != 8 {
			t.Fatalf("oversized server frame error = %v, want ResourceExhausted", err)
		}
	})
}

func finishGRPCProbe(t *testing.T, p *grpcProbe) {
	t.Helper()
	if err := p.stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.finished:
		if p.waitErr != nil {
			t.Fatalf("boundary child exit: %v", p.waitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("boundary child did not complete/reap")
	}
}

func TestM6IPCGRPCChildCrashAndConnectionDrop(t *testing.T) {
	t.Run("child crash", func(t *testing.T) {
		p := startGRPCProbe(t)
		request := append(bytes.Repeat([]byte{'h'}, ipcHeaderBytes), stdioRequestPayload()[ipcHeaderBytes:]...)
		if err := p.stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcExecute, Data: request}); err != nil {
			t.Fatal(err)
		}
		if f, err := p.stream.Recv(); err != nil || f.GetKind() != grpcCredential {
			t.Fatalf("initial child response = %+v, %v", f, err)
		}
		if err := p.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { _, err := p.stream.Recv(); result <- err }()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("crashed child produced an unexpected response")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("RPC remained blocked after child crash")
		}
		select {
		case <-p.finished:
		case <-time.After(2 * time.Second):
			t.Fatal("crashed child did not reap")
		}
		if p.waitErr == nil {
			t.Fatal("killed child unexpectedly exited successfully")
		}
	})
	t.Run("connection drop", func(t *testing.T) {
		p := startGRPCProbe(t)
		if err := p.conn.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := p.stream.Recv(); err == nil {
			t.Fatal("closed connection left RPC stream live")
		}
	})
}

func TestM6IPCGRPCTruncatedConnectionDoesNotDisturbChild(t *testing.T) {
	p := startGRPCProbe(t)
	raw, err := (&net.Dialer{}).DialContext(context.Background(), "unix", p.path)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.SetDeadline(time.Now().Add(time.Second))
	// HTTP/2 preface followed by a partial frame header, then an abrupt close.
	if _, err := raw.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00")); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	request := append(bytes.Repeat([]byte{'h'}, ipcHeaderBytes), stdioRequestPayload()[ipcHeaderBytes:]...)
	if err := p.stream.Send(&grpcspike.Frame{Correlation: grpcCallID, Kind: grpcExecute, Data: request}); err != nil {
		t.Fatal(err)
	}
	if f, err := p.stream.Recv(); err != nil || f.GetKind() != grpcCredential {
		t.Fatalf("valid RPC after truncated connection = %+v, %v", f, err)
	}
}

func TestM6IPCGRPCTenSecondWatchdogKillsReapsAndUnlinks(t *testing.T) {
	p := startGRPCProbe(t)
	started := time.Now()
	select {
	case <-p.finished:
	case <-time.After(11 * time.Second):
		t.Fatal("10-second child watchdog did not terminate the idle child")
	}
	elapsed := time.Since(started)
	if elapsed < 9*time.Second || elapsed > 11*time.Second {
		t.Fatalf("watchdog elapsed %v, want fixed 10 seconds", elapsed)
	}
	if p.waitErr == nil {
		t.Fatal("watchdog child exited without kill")
	}
	if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("watchdog socket cleanup: %v", err)
	}
	if _, err := os.Stat(p.path); !os.IsNotExist(err) {
		t.Fatalf("watchdog left socket file: %v", err)
	}
}

type grpcProbe struct {
	cmd           *exec.Cmd
	conn          *grpc.ClientConn
	stream        grpc.BidiStreamingClient[grpcspike.Frame, grpcspike.Frame]
	finished      chan struct{}
	waitErr       error
	path          string
	watchdog      *time.Timer
	cancel        context.CancelFunc
	ctx           context.Context
	events        chan string
	killOnCleanup bool
}

func startGRPCProbe(t *testing.T) *grpcProbe {
	return startGRPCProbeWithCleanup(t, true)
}

func startGRPCProbeNoKill(t *testing.T) *grpcProbe {
	return startGRPCProbeWithCleanup(t, false)
}

func startGRPCProbeWithCleanup(t *testing.T, killOnCleanup bool) *grpcProbe {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.sock")
	deadline := time.Now().Add(10 * time.Second)
	cmd := exec.Command(os.Args[0], "-test.run=^TestM6IPCGRPCChild$")
	cmd.Env = append(os.Environ(), "PESTIROUTE_M6_GRPC_CHILD=1", "PESTIROUTE_M6_GRPC_SOCKET="+path, "GORACE=atexit_sleep_ms=0")
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &grpcProbe{cmd: cmd, path: path, finished: make(chan struct{}), events: make(chan string, 64), killOnCleanup: killOnCleanup}
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			p.events <- scanner.Text()
		}
		if err := scanner.Err(); err != nil {
			p.events <- "stderr scanner error: " + err.Error()
		}
	}()
	p.watchdog = time.AfterFunc(time.Until(deadline), func() { _ = cmd.Process.Kill() })
	go func() { p.waitErr = cmd.Wait(); close(p.finished) }()
	t.Cleanup(func() {
		select {
		case <-p.finished:
		default:
			if p.cancel != nil {
				p.cancel()
			}
			if p.killOnCleanup {
				if p.conn != nil {
					_ = p.conn.Close()
				}
				_ = cmd.Process.Kill()
			}
			select {
			case <-p.finished:
			case <-time.After(12 * time.Second):
				t.Errorf("probe child did not exit/reap before watchdog bound")
			}
		}
		p.watchdog.Stop()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove probe socket: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("probe socket remains after cleanup: %v", err)
		}
	})
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	p.cancel = cancel
	p.ctx = ctx
	t.Cleanup(cancel)
	for {
		c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err == nil {
			_ = c.Close()
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("probe socket startup: %v", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	conn, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}), grpc.WithInitialWindowSize(ipcFrameBytes), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(grpcMessageLimit), grpc.MaxCallSendMsgSize(grpcMessageLimit)))
	if err != nil {
		t.Fatal(err)
	}
	p.conn = conn
	p.stream, err = grpcspike.NewBridgeClient(conn).Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

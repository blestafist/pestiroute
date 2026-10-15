//go:build linux

package external

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

type logTestCredentials struct{}

func (logTestCredentials) Get(_ context.Context, name string) ([]byte, error) {
	if name != "api_key" {
		return nil, errors.New("missing")
	}
	return []byte("secret-marker"), nil
}

type stalledSlogHandler struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *stalledSlogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *stalledSlogHandler) Handle(context.Context, slog.Record) error {
	h.once.Do(func() { close(h.started) })
	<-h.release
	return nil
}
func (h *stalledSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *stalledSlogHandler) WithGroup(string) slog.Handler      { return h }

type blockingLogHost struct {
	gen.UnimplementedHostServicesServer
	started  chan struct{}
	finished chan struct{}
}

func (s *blockingLogHost) Log(stream grpc.ClientStreamingServer[gen.RecordFragment, emptypb.Empty]) error {
	close(s.started)
	<-stream.Context().Done()
	close(s.finished)
	return stream.Context().Err()
}

type deliveredLogHost struct {
	gen.UnimplementedHostServicesServer
	invocationIDs chan string
}

func (s *deliveredLogHost) Log(stream grpc.ClientStreamingServer[gen.RecordFragment, emptypb.Empty]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	s.invocationIDs <- first.InvocationId
	for {
		if _, err := stream.Recv(); errors.Is(err, io.EOF) {
			return stream.SendAndClose(&emptypb.Empty{})
		} else if err != nil {
			return err
		}
	}
}

func TestM6LogInvocationCancellationDoesNotStopSharedDispatcher(t *testing.T) {
	processCtx, cancelProcess := context.WithCancel(context.Background())
	defer cancelProcess()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	host := &deliveredLogHost{invocationIDs: make(chan string, 1)}
	gen.RegisterHostServicesServer(server, host)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///log-isolation", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	dispatcher := NewLogDispatcher(processCtx, gen.NewHostServicesClient(conn), 22)
	t.Cleanup(func() { cancelProcess(); <-dispatcher.done })
	contexts := make([]context.CancelFunc, 4)
	loggers := make([]*slog.Logger, 4)
	for i := range loggers {
		ctx, cancel := context.WithCancel(context.Background())
		contexts[i] = cancel
		loggers[i] = slog.New(NewLogHandler(ctx, dispatcher, fmt.Sprintf("inv-%d", i)))
	}
	t.Cleanup(func() {
		for _, cancel := range contexts {
			cancel()
		}
	})
	contexts[0]()
	if loggers[0].Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("cancelled invocation remained enabled")
	}
	if !loggers[1].Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("cancelling one invocation disabled the shared dispatcher")
	}
	loggers[1].Info("still-live")
	select {
	case invocationID := <-host.invocationIDs:
		if invocationID != "inv-1" {
			t.Fatalf("delivered invocation ID = %q, want inv-1", invocationID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live invocation log was not delivered")
	}
}

func TestM6LogDispatcherStopsOnProcessCancellation(t *testing.T) {
	processCtx, cancelProcess := context.WithCancel(context.Background())
	defer cancelProcess()
	invocationCtx, cancelInvocation := context.WithCancel(context.Background())
	defer cancelInvocation()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	host := &blockingLogHost{started: make(chan struct{}), finished: make(chan struct{})}
	gen.RegisterHostServicesServer(server, host)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///log-worker", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	dispatcher := NewLogDispatcher(processCtx, gen.NewHostServicesClient(conn), 23)
	handler := NewLogHandler(invocationCtx, dispatcher, "invocation")
	logger := slog.New(handler)
	logger.Info("pending")
	select {
	case <-host.started:
	case <-time.After(2 * time.Second):
		t.Fatal("log worker did not start its RPC")
	}
	cancelInvocation()
	select {
	case <-host.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("invocation cancellation did not cancel the log RPC")
	}
	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("cancelled log handler remained enabled")
	}
	if err := handler.Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, "late", 0)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dispatcher.done:
		t.Fatal("invocation cancellation stopped the process-scoped worker")
	default:
	}
	cancelProcess()
	select {
	case <-dispatcher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("process-scoped log worker did not stop on lifecycle cancellation")
	}
}

func TestM6LogDispatcherQueueBoundAcrossInvocations(t *testing.T) {
	processCtx, cancelProcess := context.WithCancel(context.Background())
	defer cancelProcess()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	host := &blockingLogHost{started: make(chan struct{}), finished: make(chan struct{})}
	gen.RegisterHostServicesServer(server, host)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///log-queue", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	dispatcher := NewLogDispatcher(processCtx, gen.NewHostServicesClient(conn), 24)
	contexts := make([]context.CancelFunc, maxInvocations)
	loggers := make([]*childLogHandler, maxInvocations)
	for i := range loggers {
		ctx, cancel := context.WithCancel(context.Background())
		contexts[i] = cancel
		loggers[i] = NewLogHandler(ctx, dispatcher, fmt.Sprintf("inv-%d", i)).(*childLogHandler)
	}
	t.Cleanup(func() {
		for _, cancel := range contexts {
			cancel()
		}
	})
	if err := loggers[0].Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, "in-flight", 0)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.started:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not start blocked RPC")
	}
	for i, logger := range loggers {
		for n := 0; n < maxLogQueue/2; n++ {
			if err := logger.Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, fmt.Sprintf("%d-%d", i, n), 0)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := len(dispatcher.queue); got != maxLogQueue {
		t.Fatalf("aggregate queue across invocation loggers = %d, want %d", got, maxLogQueue)
	}
	for _, cancel := range contexts {
		cancel()
	}
	cancelProcess()
	select {
	case <-host.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("process shutdown did not cancel in-flight log RPC")
	}
	select {
	case <-dispatcher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not release queued records on process shutdown")
	}
	if got := len(dispatcher.queue); got != 0 {
		t.Fatalf("shutdown retained %d queued records", got)
	}
}

func TestM6LogStalledFragmentsHoldSlotsAndCancelCleanly(t *testing.T) {
	parent, cancelInvocation := context.WithCancel(context.Background())
	defer cancelInvocation()
	registry := newInvocationRegistry(19)
	id, _, err := registry.begin(parent, "account")
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	hostCtx, cancelHost := context.WithCancel(context.Background())
	service := newLogService(hostCtx, registry, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	gen.RegisterHostServicesServer(server, service)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	t.Cleanup(func() { cancelHost(); <-service.logWorkerDone })
	conn, err := grpc.NewClient("passthrough:///log-stall", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := gen.NewHostServicesClient(conn)
	wire, err := ipc.LogRecordTo("INFO", "held", nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	open := make([]context.CancelFunc, 0, maxCallbacksPerInvoke)
	streams := make([]gen.HostServices_LogClient, 0, maxCallbacksPerInvoke)
	for i := 0; i < maxCallbacksPerInvoke; i++ {
		rpcCtx, cancel := context.WithCancel(context.Background())
		stream, streamErr := client.Log(rpcCtx)
		if streamErr != nil {
			cancel()
			t.Fatal(streamErr)
		}
		callID := hostServiceCallID.Add(1)
		fragment := &gen.RecordFragment{Epoch: 19, CallId: callID, RecordType: "LogRecord", TotalLength: uint64(len(encoded)), Data: encoded[:1], InvocationId: id}
		if sendErr := stream.Send(fragment); sendErr != nil {
			cancel()
			t.Fatal(sendErr)
		}
		open = append(open, cancel)
		streams = append(streams, stream)
	}
	t.Cleanup(func() {
		for _, cancel := range open {
			cancel()
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		registry.mu.Lock()
		outstanding := registry.outstanding
		registry.mu.Unlock()
		if outstanding == maxCallbacksPerInvoke {
			break
		}
		time.Sleep(time.Millisecond)
	}
	registry.mu.Lock()
	outstanding := registry.outstanding
	registry.mu.Unlock()
	if outstanding != maxCallbacksPerInvoke {
		t.Fatalf("stalled log RPCs did not retain callback slots: %d", outstanding)
	}
	denied, err := client.Log(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := denied.Send(&gen.RecordFragment{Epoch: 19, CallId: hostServiceCallID.Add(1), RecordType: "LogRecord", TotalLength: uint64(len(encoded)), Data: encoded[:1], InvocationId: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := denied.CloseAndRecv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ninth callback was not rejected at invocation limit: %v", err)
	}
	cancelInvocation()
	for _, stream := range streams {
		if _, err := stream.CloseAndRecv(); status.Code(err) != codes.Canceled {
			t.Fatalf("cancelled log RPC did not terminate: %v", err)
		}
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		registry.mu.Lock()
		outstanding = registry.outstanding
		registry.mu.Unlock()
		if outstanding == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	registry.mu.Lock()
	outstanding = registry.outstanding
	remaining := len(registry.calls)
	registry.mu.Unlock()
	if outstanding != 0 || remaining != 0 {
		t.Fatalf("invocation cancellation leaked callback resources: outstanding=%d calls=%d", outstanding, remaining)
	}
}

func TestM6LogStructuredDeliveryRedactsInvocationSecret(t *testing.T) {
	registry := newInvocationRegistry(9)
	id, _, err := registry.beginWithCredentials(context.Background(), "account", logTestCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	output := &lockedLogBuffer{}
	logger := slog.New(slog.NewTextHandler(output, nil))
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	hostCtx, cancelHost := context.WithCancel(context.Background())
	service := newLogService(hostCtx, registry, logger)
	gen.RegisterHostServicesServer(server, service)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	t.Cleanup(func() { cancelHost(); <-service.logWorkerDone })
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	credentialClient := NewCredentialClient(gen.NewHostServicesClient(conn), 9, id)
	if _, err := credentialClient.Get(context.Background(), "api_key"); err != nil {
		t.Fatalf("credential callback failed before log callback: %v", err)
	}
	logCtx, cancelLogs := context.WithCancel(context.Background())
	dispatcher := NewLogDispatcher(logCtx, gen.NewHostServicesClient(conn), 9)
	t.Cleanup(func() { cancelLogs(); <-dispatcher.done })
	childLogger := slog.New(NewLogHandler(context.Background(), dispatcher, id))
	childLogger.Info("request secret-marker", "operation", "execute", "detail", "secret-marker")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), "[REDACTED]") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	got := output.String()
	if !strings.Contains(got, "operation=execute") || !strings.Contains(got, "detail=") || !strings.Contains(got, "[REDACTED]") || strings.Contains(got, "secret-marker") {
		t.Fatalf("log not structured/redacted: %q", got)
	}
}

type lockedLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}
func (b *lockedLogBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.Buffer.String() }

func TestM6LogQueueDropsExcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	blocker := &stalledSlogHandler{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocker.release) }) }
	t.Cleanup(release)
	service := newLogService(ctx, newInvocationRegistry(1), slog.New(blocker))
	service.logQueue <- logEntry{}
	select {
	case <-blocker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("host logger worker did not enter blocking handler")
	}
	for i := 0; i < maxLogQueue+8; i++ {
		select {
		case service.logQueue <- logEntry{}:
		default:
		}
	}
	if len(service.logQueue) != maxLogQueue {
		t.Fatalf("queue grew beyond cap: %d", len(service.logQueue))
	}
	cancel()
	release()
	<-service.logWorkerDone
}

func TestM6LogHostQueueWorkerStopsWithLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	service := newLogService(ctx, newInvocationRegistry(1), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	cancel()
	select {
	case <-service.logWorkerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("host log queue worker survived service shutdown")
	}
}

func TestM6LogDiagnosticsRedactsCredentialMarker(t *testing.T) {
	process := &Process{diagnostics: &limitedWriter{}, secrets: &logSecretSet{}}
	process.secrets.add([]byte("diagnostic-secret"))
	_, _ = process.diagnostics.Write([]byte("child diagnostic-secret output"))
	if got := process.Diagnostics(); strings.Contains(got, "diagnostic-secret") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("diagnostic secret leaked: %q", got)
	}
}

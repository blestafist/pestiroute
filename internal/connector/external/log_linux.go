//go:build linux

package external

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const maxLogQueue = 256

type logEntry struct {
	logger *slog.Logger
	inv    *invocation
	level  slog.Level
	msg    string
	attrs  map[string]string
}

func newLogService(ctx context.Context, registry *invocationRegistry, logger *slog.Logger) *hostServices {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &hostServices{registry: registry, logger: logger, logQueue: make(chan logEntry, maxLogQueue), logWorkerDone: make(chan struct{})}
	go func() {
		defer close(s.logWorkerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case entry := <-s.logQueue:
				if ctx.Err() != nil {
					return
				}
				logger := entry.logger
				if logger == nil {
					logger = s.logger
				}
				attrs := make([]any, 0, len(entry.attrs)*2)
				for key, value := range entry.attrs {
					attrs = append(attrs, key, value)
				}
				logger.Log(context.Background(), entry.level, entry.msg, attrs...)
			}
		}
	}()
	return s
}

func (s *hostServices) Log(stream grpc.ClientStreamingServer[gen.RecordFragment, emptypb.Empty]) error {
	first, err := stream.Recv()
	if err != nil || first == nil || first.RecordId != 0 || first.TotalLength > ipc.MaxLogRecordBytes {
		return status.Error(codes.InvalidArgument, "invalid log record")
	}
	identity := ipc.Identity{Epoch: first.Epoch, CallID: first.CallId, RecordID: first.RecordId, Type: "LogRecord"}
	invocationID := first.InvocationId
	var lease *callbackLease
	var inv *invocation
	if invocationID != "" {
		var admitErr error
		lease, _, _, admitErr = s.registry.admitCallback(identity.Epoch, invocationID, identity.CallID)
		if admitErr != nil {
			return status.Error(codes.PermissionDenied, "log denied")
		}
		defer lease.release()
		inv = lease.invocation
	} else if identity.Epoch != s.registry.epoch || identity.CallID == 0 {
		return status.Error(codes.PermissionDenied, "log denied")
	}
	assembler := ipc.NewAssembler(identity)
	if assembler.Add(first) != nil {
		return status.Error(codes.InvalidArgument, "invalid log record")
	}
	for !assembler.Complete() {
		fragment, recvErr := recvLogFragment(stream.Context(), inv, stream)
		if recvErr != nil || fragment == nil || fragment.InvocationId != invocationID || fragment.Epoch != identity.Epoch || fragment.CallId != identity.CallID || fragment.TotalLength > ipc.MaxLogRecordBytes || assembler.Add(fragment) != nil {
			if inv != nil && inv.ctx.Err() != nil {
				return status.Error(codes.Canceled, "log cancelled")
			}
			return status.Error(codes.InvalidArgument, "invalid log record")
		}
	}
	var wire gen.LogRecord
	if assembler.Decode(&wire) != nil {
		return status.Error(codes.InvalidArgument, "invalid log record")
	}
	level, message, attrs, decodeErr := ipc.LogRecordFrom(&wire)
	if decodeErr != nil {
		return status.Error(codes.InvalidArgument, "invalid log record")
	}
	entry := logEntry{logger: s.logger, msg: message, attrs: attrs}
	switch level {
	case "DEBUG":
		entry.level = slog.LevelDebug
	case "INFO":
		entry.level = slog.LevelInfo
	case "WARN":
		entry.level = slog.LevelWarn
	case "ERROR":
		entry.level = slog.LevelError
	}
	if inv != nil {
		entry.logger = invocationLogger(inv, s.logger)
	}
	// One logical log record per RPC. Do not accept or enqueue late/extra data.
	if extra, recvErr := recvLogFragment(stream.Context(), inv, stream); !errors.Is(recvErr, io.EOF) || extra != nil {
		if inv != nil && inv.ctx.Err() != nil {
			return status.Error(codes.Canceled, "log cancelled")
		}
		return status.Error(codes.InvalidArgument, "invalid log record")
	}
	if lease != nil {
		if _, err := enqueueInvocationLog(lease, s.logQueue, entry); err != nil {
			return status.Error(codes.Canceled, "log cancelled")
		}
	} else {
		select {
		case s.logQueue <- entry:
		default: // Best effort: a full process queue drops this record.
		}
	}
	return stream.SendAndClose(&emptypb.Empty{})
}

func enqueueInvocationLog(lease *callbackLease, queue chan<- logEntry, entry logEntry) (bool, error) {
	r := lease.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	inv := lease.invocation
	if inv.ctx.Err() != nil || inv.revoked || r.active[inv.id] != inv {
		return false, errInvocationStale
	}
	if _, active := inv.callbacks[lease.callID]; !active {
		return false, errInvocationStale
	}
	select {
	case queue <- entry:
		return true, nil
	default:
		return false, nil
	}
}

func recvLogFragment(ctx context.Context, inv *invocation, stream grpc.ClientStreamingServer[gen.RecordFragment, emptypb.Empty]) (*gen.RecordFragment, error) {
	if inv == nil {
		return stream.Recv()
	}
	type result struct {
		fragment *gen.RecordFragment
		err      error
	}
	read := make(chan result, 1)
	go func() {
		fragment, err := stream.Recv()
		read <- result{fragment: fragment, err: err}
	}()
	select {
	case received := <-read:
		return received.fragment, received.err
	case <-inv.ctx.Done():
		return nil, inv.ctx.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func invocationLogger(inv *invocation, fallback *slog.Logger) *slog.Logger {
	base := inv.logger
	if base == nil {
		base = fallback
	}
	if base == nil {
		base = slog.Default()
	}
	return slog.New(secretRedactingHandler{next: base.Handler(), secrets: inv.secrets})
}

type logSecretSet struct {
	mu     sync.RWMutex
	values []string
}

func (s *logSecretSet) add(value []byte) {
	if len(value) == 0 {
		return
	}
	s.mu.Lock()
	s.values = append(s.values, string(value))
	s.mu.Unlock()
}

func (s *logSecretSet) redact(value string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, secret := range s.values {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return value
}

type secretRedactingHandler struct {
	next    slog.Handler
	secrets *logSecretSet
}

func (h secretRedactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}
func (h secretRedactingHandler) Handle(ctx context.Context, record slog.Record) error {
	attrs := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool { attrs = append(attrs, h.redactAttr(attr)); return true })
	record = slog.NewRecord(record.Time, record.Level, h.secrets.redact(record.Message), record.PC)
	record.AddAttrs(attrs...)
	return h.next.Handle(ctx, record)
}
func (h secretRedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		redacted[i] = h.redactAttr(attr)
	}
	return secretRedactingHandler{next: h.next.WithAttrs(redacted), secrets: h.secrets}
}
func (h secretRedactingHandler) WithGroup(name string) slog.Handler {
	return secretRedactingHandler{next: h.next.WithGroup(name), secrets: h.secrets}
}
func (h secretRedactingHandler) redactAttr(attr slog.Attr) slog.Attr {
	attr.Key = h.secrets.redact(attr.Key)
	attr.Value = attr.Value.Resolve()
	switch attr.Value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(h.secrets.redact(attr.Value.String()))
	case slog.KindGroup:
		group := attr.Value.Group()
		for i := range group {
			group[i] = h.redactAttr(group[i])
		}
		attr.Value = slog.GroupValue(group...)
	}
	return attr
}

// LogDispatcher owns one process-wide queue and worker. Create one per child
// process; invocation handlers created from it share its 256-record bound.
func NewLogDispatcher(ctx context.Context, client gen.HostServicesClient, epoch uint64) *LogDispatcher {
	if ctx == nil {
		ctx = context.Background()
	}
	d := &LogDispatcher{ctx: ctx, client: client, epoch: epoch, queue: make(chan queuedLogRecord, maxLogQueue), done: make(chan struct{})}
	go d.run()
	return d
}

// NewLogHandler creates one invocation's structured logger on a shared process
// dispatcher. Queue items retain only the invocation's cancellation channel.
func NewLogHandler(ctx context.Context, dispatcher *LogDispatcher, invocationID string) slog.Handler {
	if ctx == nil {
		ctx = context.Background()
	}
	return &childLogHandler{dispatcher: dispatcher, ctx: ctx, invocationID: invocationID}
}

type queuedLogRecord struct {
	record         *gen.LogRecord
	invocationID   string
	invocationDone <-chan struct{}
}

type LogDispatcher struct {
	ctx     context.Context
	client  gen.HostServicesClient
	epoch   uint64
	queue   chan queuedLogRecord
	done    chan struct{}
	mu      sync.Mutex
	stopped bool
}

type childLogHandler struct {
	dispatcher   *LogDispatcher
	ctx          context.Context
	invocationID string
	attrs        map[string]string
	group        string
}

func (h *childLogHandler) Enabled(context.Context, slog.Level) bool {
	d := h.dispatcher
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil {
		d.stopped = true
	}
	return !d.stopped && h.ctx.Err() == nil && d.client != nil && h.invocationID != ""
}
func (h *childLogHandler) Handle(_ context.Context, record slog.Record) error {
	d := h.dispatcher
	d.mu.Lock()
	if d.stopped || d.ctx.Err() != nil || h.ctx.Err() != nil || d.client == nil || h.invocationID == "" {
		if d.ctx.Err() != nil {
			d.stopped = true
		}
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()
	attrs := make(map[string]string, len(h.attrs)+record.NumAttrs())
	for key, value := range h.attrs {
		attrs[key] = value
	}
	record.Attrs(func(attr slog.Attr) bool {
		flattenLogAttr(attrs, h.group, attr)
		return true
	})
	level := "INFO"
	switch {
	case record.Level <= slog.LevelDebug:
		level = "DEBUG"
	case record.Level >= slog.LevelError:
		level = "ERROR"
	case record.Level >= slog.LevelWarn:
		level = "WARN"
	}
	wire, err := ipc.LogRecordTo(level, record.Message, attrs)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.ctx.Err() != nil || h.ctx.Err() != nil || d.client == nil || h.invocationID == "" {
		if d.ctx.Err() != nil {
			d.stopped = true
		}
		return nil
	}
	select {
	case d.queue <- queuedLogRecord{record: wire, invocationID: h.invocationID, invocationDone: h.ctx.Done()}:
	case <-h.ctx.Done():
	default:
	}
	return nil
}
func (h *childLogHandler) WithAttrs(extra []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = make(map[string]string, len(h.attrs)+len(extra))
	for key, value := range h.attrs {
		clone.attrs[key] = value
	}
	for _, attr := range extra {
		flattenLogAttr(clone.attrs, clone.group, attr)
	}
	return &clone
}
func (h *childLogHandler) WithGroup(group string) slog.Handler {
	clone := *h
	if clone.group != "" && group != "" {
		clone.group += "."
	}
	clone.group += group
	return &clone
}

func flattenLogAttr(dst map[string]string, prefix string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	key := attr.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	if attr.Value.Kind() == slog.KindGroup {
		for _, child := range attr.Value.Group() {
			flattenLogAttr(dst, key, child)
		}
		return
	}
	if _, err := ipc.LogRecordTo("INFO", "", map[string]string{key: ""}); err == nil {
		dst[key] = attr.Value.String()
	}
}
func (d *LogDispatcher) run() {
	defer close(d.done)
	defer func() {
		d.mu.Lock()
		d.stopped = true
		d.mu.Unlock()
		for {
			select {
			case <-d.queue:
			default:
				return
			}
		}
	}()
	for {
		if d.ctx.Err() != nil {
			return
		}
		select {
		case <-d.ctx.Done():
			return
		case queued := <-d.queue:
			if d.ctx.Err() != nil {
				return
			}
			if invocationEnded(queued.invocationDone) {
				continue
			}
			ctx, cancel := context.WithTimeout(d.ctx, 2*time.Second)
			stopWatch := make(chan struct{})
			if queued.invocationDone != nil {
				go func(done <-chan struct{}) {
					select {
					case <-done:
						cancel()
					case <-stopWatch:
					}
				}(queued.invocationDone)
			}
			stream, err := d.client.Log(ctx)
			callID := hostServiceCallID.Add(1)
			if err == nil {
				err = ipc.WriteRecord(ipc.Identity{Epoch: d.epoch, CallID: callID, Type: "LogRecord"}, queued.record, func(fragment *gen.RecordFragment) error {
					fragment.InvocationId = queued.invocationID
					return stream.Send(fragment)
				})
			}
			if err == nil {
				_, err = stream.CloseAndRecv()
			}
			close(stopWatch)
			cancel()
		}
	}
}

func invocationEnded(done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

var _ gen.HostServicesServer = (*hostServices)(nil)

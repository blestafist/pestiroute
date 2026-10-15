//go:build linux

package external

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/ipc"
	gen "github.com/blestafist/pestiroute/internal/ipc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func residentBytes(t *testing.T, pid int) int64 {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			kb, err := strconv.ParseInt(strings.Fields(line)[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return kb * 1024
		}
	}
	t.Fatal("process has no resident-memory sample")
	return 0
}

// This deliberately enlarges the real data connection's HTTP/2 window: a small
// transport window must not be the reason that a stalled producer stays bounded.
func TestM6ResponsesChildAggregateStalledConsumers(t *testing.T) {
	started, stopped := make(chan struct{}, 4), make(chan struct{}, 4)
	block := bytes.Repeat([]byte(": opaque padding\n"), 2048)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { stopped <- struct{}{} }()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		started <- struct{}{}
		for {
			if _, err := w.Write(block); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			if r.Context().Err() != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	process, child := startResponsesChild(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel) // Runs before child and upstream cleanup, including failures.
	if err := child.Init(ctx, nativeResponsesConfig(t, upstream.URL+"/v1/responses")); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient("passthrough:///large-window-child",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", process.connector)
		}), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(32<<20), grpc.WithInitialConnWindowSize(32<<20),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(ipc.MaxGRPCMessageBytes), grpc.MaxCallSendMsgSize(ipc.MaxGRPCMessageBytes)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	child.client = gen.NewConnectorClient(conn)
	baselineParent, baselineChild := residentBytes(t, os.Getpid()), residentBytes(t, process.PID())
	type result struct {
		response core.ExecutionResponse
		err      *core.GatewayError
	}
	results := make(chan result, 4)
	streaming := true
	for i := range 4 {
		go func() {
			response, ge := child.Execute(ctx, core.ExecutionRequest{Model: "fixture-model", Metadata: core.RequestMetadata{Streaming: &streaming}, Payload: core.RawPayload{Protocol: "openai.responses.v1", Body: []byte(`{"model":"fixture-model","stream":true}`)}}, core.AttemptScope{ID: fmt.Sprintf("aggregate-%d", i), AccountID: "fixture-account", Mode: core.ModeNative, MaxBodyBytes: 1024}, testHTTPServices())
			results <- result{response, ge}
		}()
	}
	for range 4 {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatal(r.err)
			}
			t.Cleanup(func() { _ = r.response.Stream.Close() })
			frame, err := r.response.Stream.Next(ctx)
			if err != nil || frame.Type != core.FrameHead {
				t.Fatalf("head=%+v error=%v", frame, err)
			}
		case <-ctx.Done():
			t.Fatal("four real-child executions did not start")
		}
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("upstream callback did not start")
		}
	}
	broker := child.invocations.credits
	for {
		frames, _ := broker.usage()
		if frames == maxQueueFramesProcess {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("real child never saturated aggregate broker: frames=%d", frames)
		case <-time.After(10 * time.Millisecond):
		}
	}
	peakParent, peakChild := int64(0), int64(0)
	for sample := range 20 { // A sustained one-second stall, not a single snapshot.
		broker.mu.Lock()
		frames, queued := broker.frames, broker.bytes
		for id, usage := range broker.active {
			if usage.frames == 0 || usage.frames > maxQueueFramesPerInvocation || usage.bytes > maxQueueBytesPerInvocation {
				frames, queued := usage.frames, usage.bytes
				broker.mu.Unlock()
				t.Fatalf("invocation %s escaped stalled bound: frames=%d bytes=%d", id, frames, queued)
			}
			if sample == 0 {
				t.Logf("stalled invocation %s: frames=%d bytes=%d", id, usage.frames, usage.bytes)
			}
		}
		broker.mu.Unlock()
		if frames == 0 || frames > maxQueueFramesProcess || queued > maxQueueBytesProcess {
			t.Fatalf("stalled aggregate escaped bounds: frames=%d bytes=%d", frames, queued)
		}
		parentRSS, childRSS := residentBytes(t, os.Getpid()), residentBytes(t, process.PID())
		peakParent, peakChild = max(peakParent, parentRSS), max(peakChild, childRSS)
		t.Logf("stall sample=%d frames=%d queued_bytes=%d parent_rss=%d child_rss=%d", sample, frames, queued, parentRSS, childRSS)
		time.Sleep(50 * time.Millisecond)
	}
	// RSS includes gRPC/runtime/race-detector allocations outside queued payloads;
	// its explicit allowance is separate from the strict 16 MiB broker ceiling.
	const rssGrowthAllowance = 128 << 20
	t.Logf("baseline parent_rss=%d child_rss=%d; peak parent_rss=%d child_rss=%d; growth allowance=%d per process", baselineParent, baselineChild, peakParent, peakChild, rssGrowthAllowance)
	if peakParent-baselineParent > rssGrowthAllowance || peakChild-baselineChild > rssGrowthAllowance {
		t.Fatal("resident growth exceeded the real-child stalled-stream budget")
	}
	cancelStart := time.Now()
	cancel()
	for range 4 {
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Fatal("saturated real-child cancellation did not release HTTP callback")
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		frames, queued := broker.usage()
		child.invocations.mu.Lock()
		active := len(child.invocations.active)
		child.invocations.mu.Unlock()
		if frames == 0 && queued == 0 && active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancel retained credits/authority: frames=%d bytes=%d active=%d", frames, queued, active)
		}
		time.Sleep(time.Millisecond)
	}
	t.Logf("all four callbacks and aggregate credits released after %s", time.Since(cancelStart))
	if err := child.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := process.Shutdown(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(process.dir); !os.IsNotExist(err) {
		t.Fatalf("owned socket directory survived shutdown: %v", err)
	}
}

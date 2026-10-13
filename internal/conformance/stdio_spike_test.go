package conformance

// This disposable test-only protocol is deliberately not a production binding.
// Frames are uint32 big-endian length + kind + uint64 invocation ID + opaque
// payload; each call permits one uncredited output frame and one callback frame.
import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	stdioHead byte = iota + 1
	stdioBody
	stdioCredential
	stdioHTTP
	stdioHTTPReply
	stdioContinue
	stdioComplete
	stdioStop
	stdioExecute      byte = 0x10
	stdioFrameLimit        = 84 << 10
	ipcHTTPChunk           = 16 << 10
	stdioCallID            = 0x4d362d3030330001
	stdioQueueLimit        = ipcFrameBytes + ipcHTTPChunk + ipcHeaderBytes + 26 // Body + callback header/body frames.
	stdioRunDeadline       = 10 * time.Second
	stdioReapDeadline      = 2 * time.Second
)

type stdioFrame struct {
	kind byte
	body []byte
}

func writeStdio(w io.Writer, f stdioFrame) error {
	n := len(f.body) + 9
	if n+4 > stdioFrameLimit {
		return fmt.Errorf("encoded frame %d exceeds %d", n, stdioFrameLimit)
	}
	var h [13]byte
	binary.BigEndian.PutUint32(h[:4], uint32(n))
	h[4] = f.kind
	binary.BigEndian.PutUint64(h[5:], stdioCallID)
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(f.body)
	return err
}

func readStdio(r io.Reader) (stdioFrame, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return stdioFrame{}, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n < 9 || n+4 > stdioFrameLimit {
		return stdioFrame{}, fmt.Errorf("invalid frame length %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return stdioFrame{}, err
	}
	if (b[0] < stdioHead || b[0] > stdioStop) && b[0] != stdioExecute {
		return stdioFrame{}, fmt.Errorf("unknown frame kind %d", b[0])
	}
	if binary.BigEndian.Uint64(b[1:9]) != stdioCallID {
		return stdioFrame{}, fmt.Errorf("unknown invocation correlation %x", b[1:9])
	}
	return stdioFrame{b[0], b[9:]}, nil
}

func stdioRequestPayload() []byte {
	header := bytes.Repeat([]byte{'h'}, ipcHeaderBytes)
	return append(header, append([]byte("unknown-field\x00\xff"), opaqueRequest...)...)
}

func receiveStdioExecute(r io.Reader) bool {
	f, err := readStdio(r)
	if err != nil || f.kind != stdioExecute || len(f.body) < ipcHeaderBytes {
		return false
	}
	for _, b := range f.body[:ipcHeaderBytes] {
		if b != 'h' {
			return false
		}
	}
	want := append([]byte("unknown-field\x00\xff"), opaqueRequest...)
	return bytes.Equal(f.body[ipcHeaderBytes:], want)
}

func TestM6StdioChild(t *testing.T) {
	if os.Getenv("PESTIROUTE_M6_STDIO_CHILD") != "1" {
		return
	}
	// The helper writes protocol bytes to stdout only; no test/log output there.
	r := bufio.NewReader(os.Stdin)
	w := bufio.NewWriter(os.Stdout)
	put := func(k byte, b []byte) {
		if err := writeStdio(w, stdioFrame{k, b}); err != nil {
			os.Exit(31)
		}
		if w.Flush() != nil {
			os.Exit(32)
		}
	}
	switch os.Getenv("PESTIROUTE_M6_STDIO_MODE") {
	case "crash":
		os.Exit(17)
	case "truncated":
		_, _ = os.Stdout.Write([]byte{0, 0})
		os.Exit(0)
	case "oversize":
		_, _ = os.Stdout.Write([]byte{0, 1, 0x50, 0x01})
		os.Exit(0)
	case "malformed":
		_, _ = os.Stdout.Write([]byte{0, 0, 0, 1, 0})
		os.Exit(0)
	}
	if os.Getenv("PESTIROUTE_M6_STDIO_MODE") == "cancel" {
		if !receiveStdioExecute(r) {
			fmt.Fprintln(os.Stderr, "invalid Execute")
			os.Exit(29)
		}
		put(stdioHead, nil)
		put(stdioBody, make([]byte, ipcFrameBytes))
		credit, err := readStdio(r)
		if err != nil || credit.kind != stdioContinue {
			os.Exit(28)
		}
		chunk := make([]byte, ipcFrameBytes)
		copy(chunk, []byte{'b', 'i', 'n', 'a', 'r', 'y', 0, 0xff})
		put(stdioBody, chunk)
		callbackBody := append([]byte{0, 0xff}, bytes.Repeat([]byte{'c'}, ipcHTTPChunk-2)...)
		put(stdioHTTP, append(bytes.Repeat([]byte{'h'}, ipcHeaderBytes), callbackBody...))
		stop, err := readStdio(r)
		if err != nil || stop.kind != stdioStop || !bytes.Equal(stop.body, []byte{3}) {
			os.Exit(27)
		}
		os.Exit(0)
	}
	if !receiveStdioExecute(r) {
		fmt.Fprintln(os.Stderr, "invalid Execute")
		os.Exit(29)
	}
	if os.Getenv("PESTIROUTE_M6_STDIO_MODE") == "stall" {
		_, _ = io.Copy(io.Discard, r)
		os.Exit(0)
	}
	put(stdioCredential, []byte("bearer"))
	if f, e := readStdio(r); e != nil || f.kind != stdioCredential {
		fmt.Fprintln(os.Stderr, "stdio child initial credential response invalid", e)
		os.Exit(30)
	}
	chunk := make([]byte, ipcFrameBytes)
	copy(chunk, []byte{'b', 'i', 'n', 'a', 'r', 'y', 0, 0xff})
	put(stdioHead, nil)
	put(stdioBody, chunk)
	if f, err := readStdio(r); err != nil || f.kind != stdioContinue {
		os.Exit(26)
	}
	put(stdioBody, chunk)
	put(stdioCredential, []byte("bearer"))
	cred, err := readStdio(r)
	if err != nil || cred.kind != stdioCredential {
		fmt.Fprintln(os.Stderr, "stdio child reverse credential response invalid", err)
		os.Exit(33)
	}
	callback := append([]byte{0, 0xff}, bytes.Repeat([]byte{'c'}, ipcCallbackSize-2)...)
	for offset := 0; offset < len(callback); offset += ipcHTTPChunk {
		end := offset + ipcHTTPChunk
		if end > len(callback) {
			end = len(callback)
		}
		part := callback[offset:end]
		requestChunk := part
		if offset == 0 {
			requestChunk = append(bytes.Repeat([]byte{'h'}, ipcHeaderBytes), part...)
		}
		put(stdioHTTP, requestChunk)
		reply, readErr := readStdio(r)
		if readErr != nil || reply.kind != stdioHTTPReply || !bytes.Equal(reply.body, part) {
			fmt.Fprintln(os.Stderr, "stdio child HTTP response chunk invalid", readErr)
			os.Exit(34)
		}
	}
	if credit, err := readStdio(r); err != nil || credit.kind != stdioContinue {
		fmt.Fprintln(os.Stderr, "stdio child stream credit invalid", err)
		os.Exit(35)
	}
	for i := 2; i < ipcFrames; i++ {
		put(stdioBody, chunk)
		if _, err = readStdio(r); err != nil {
			fmt.Fprintln(os.Stderr, "stdio child body credit invalid", i, err)
			os.Exit(36)
		}
	}
	put(stdioComplete, nil)
	os.Exit(0)
}

func startStdioChild(t *testing.T, mode string) (*exec.Cmd, *bufio.Reader, io.WriteCloser, <-chan error) {
	return startStdioChildWithDeadline(t, mode, stdioRunDeadline)
}

func startStdioChildWithDeadline(t *testing.T, mode string, budget time.Duration) (*exec.Cmd, *bufio.Reader, io.WriteCloser, <-chan error) {
	t.Helper()
	deadline := time.Now().Add(budget)
	cmd := exec.Command(os.Args[0], "-test.run=^TestM6StdioChild$")
	cmd.Env = append(os.Environ(), "PESTIROUTE_M6_STDIO_CHILD=1", "PESTIROUTE_M6_STDIO_MODE="+mode, "GORACE=atexit_sleep_ms=0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { done <- cmd.Wait(); close(finished) }()
	remaining := time.Until(deadline)
	timer := time.AfterFunc(remaining, func() { _ = cmd.Process.Kill() })
	t.Cleanup(func() {
		timer.Stop()
		_ = stdin.Close()
		select {
		case <-finished:
			return
		default:
			_ = cmd.Process.Kill()
		}
		select {
		case <-finished:
		case <-time.After(stdioReapDeadline):
			t.Errorf("child PID %d did not reap during cleanup", cmd.Process.Pid)
		}
	})
	return cmd, bufio.NewReader(stdout), stdin, done
}

type stdioMeasure struct {
	first, timeNanos, reapNanos, cpuNanos, rssBytes, queueBytes int64
	queueFrames                                                 int
	throughput                                                  float64
}

func procPeakRSS(pid int) int64 {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			f := strings.Fields(line)
			if len(f) > 1 {
				n, _ := strconv.ParseInt(f[1], 10, 64)
				return n * 1024
			}
		}
	}
	return 0
}

func runStdioScenario(t *testing.T, stall <-chan struct{}, arrived chan<- struct{}) stdioMeasure {
	start := time.Now()
	cmd, r, w, done := startStdioChild(t, "scenario")
	defer func() { _ = w.Close() }()
	if err := writeStdio(w, stdioFrame{stdioExecute, stdioRequestPayload()}); err != nil {
		t.Fatal(err)
	}
	first := int64(0)
	var outputBytes int
	bodyFrames := 0
	var pendingOutput []byte
	hash := sha256.New()
	chunk := make([]byte, ipcFrameBytes)
	copy(chunk, []byte{'b', 'i', 'n', 'a', 'r', 'y', 0, 0xff})
	want := sha256.New()
	for i := 0; i < ipcFrames; i++ {
		_, _ = want.Write(chunk)
	}
	wantHash := want.Sum(nil)
	var queued, maxQueued int64
	queuedFrames, maxQueuedFrames := 0, 0
	var cpu, rss int64
	callbackChunks := 0
	credentialCalls := 0
	lastKind := byte(0)
	for {
		f, err := readStdio(r)
		if err != nil {
			select {
			case childErr := <-done:
				t.Fatalf("child frame after kind=%d bytes=%d: %v (child exit %v)", lastKind, outputBytes, err, childErr)
			default:
				t.Fatalf("child frame after kind=%d bytes=%d: %v", lastKind, outputBytes, err)
			}
		}
		lastKind = f.kind
		switch f.kind {
		case stdioHead:
		case stdioCredential:
			credentialCalls++
			if string(f.body) != "bearer" {
				t.Fatalf("credential scope %q", f.body)
			}
			if err = writeStdio(w, stdioFrame{stdioCredential, []byte("synthetic-only")}); err != nil {
				t.Fatal(err)
			}
		case stdioBody:
			if len(f.body) != ipcFrameBytes {
				t.Fatalf("body frame %d", len(f.body))
			}
			bodyFrames++
			if first == 0 {
				first = time.Since(start).Nanoseconds()
			}
			if bodyFrames == 2 {
				pendingOutput = f.body
				queued += int64(len(f.body) + 13)
				queuedFrames++
				if queued > maxQueued {
					maxQueued = queued
				}
				if queuedFrames > maxQueuedFrames {
					maxQueuedFrames = queuedFrames
				}
				if queued > stdioQueueLimit || queuedFrames > 2 {
					t.Fatalf("actual outstanding queue exceeds bound: %d bytes / %d frames", queued, queuedFrames)
				}
				break
			}
			_, _ = hash.Write(f.body)
			outputBytes += len(f.body)
			if outputBytes == ipcTotalBytes {
				rss = procPeakRSS(cmd.Process.Pid)
			}
			if outputBytes == ipcTotalBytes && rss == 0 {
				t.Fatal("child peak RSS unavailable while process active")
			}
			if queued > stdioQueueLimit || queuedFrames > 2 {
				t.Fatalf("actual outstanding queue exceeds bound: %d bytes / %d frames", queued, queuedFrames)
			}
			if bodyFrames == 1 {
				if err = writeStdio(w, stdioFrame{stdioContinue, nil}); err != nil {
					t.Fatal(err)
				}
			} else if err = writeStdio(w, stdioFrame{stdioContinue, nil}); err != nil {
				t.Fatal(err)
			}
		case stdioHTTP:
			expected := append([]byte{0, 0xff}, bytes.Repeat([]byte{'c'}, ipcCallbackSize-2)...)
			part := f.body
			if callbackChunks == 0 {
				wantHeader := bytes.Repeat([]byte{'h'}, ipcHeaderBytes)
				if len(part) < ipcHeaderBytes || !bytes.Equal(part[:ipcHeaderBytes], wantHeader) {
					t.Fatal("reverse HTTP 1 KiB header mismatch")
				}
				part = part[ipcHeaderBytes:]
			}
			startAt := callbackChunks * ipcHTTPChunk
			endAt := startAt + ipcHTTPChunk
			if endAt > len(expected) {
				endAt = len(expected)
			}
			if startAt >= len(expected) || !bytes.Equal(part, expected[startAt:endAt]) {
				t.Fatalf("callback request chunk %d mismatch: %d bytes", callbackChunks, len(part))
			}
			callbackChunks++
			queued += int64(len(f.body) + 13)
			queuedFrames++
			if queued > maxQueued {
				maxQueued = queued
			}
			if queuedFrames > maxQueuedFrames {
				maxQueuedFrames = queuedFrames
			}
			if queued > stdioQueueLimit || queuedFrames > 2 {
				t.Fatalf("actual outstanding queue exceeds bound: %d bytes / %d frames", queued, queuedFrames)
			}
			if callbackChunks == 1 && stall != nil {
				arrived <- struct{}{}
				<-stall
			}
			if err = writeStdio(w, stdioFrame{stdioHTTPReply, part}); err != nil {
				t.Fatal(err)
			}
			queued -= int64(len(f.body) + 13)
			queuedFrames--
			if callbackChunks == ipcCallbackSize/ipcHTTPChunk {
				if len(pendingOutput) != ipcFrameBytes {
					t.Fatal("callback released without queued output Body")
				}
				_, _ = hash.Write(pendingOutput)
				outputBytes += len(pendingOutput)
				pendingOutput = nil
				queued -= int64(ipcFrameBytes + 13)
				queuedFrames--
				if err = writeStdio(w, stdioFrame{stdioContinue, nil}); err != nil {
					t.Fatal(err)
				}
			}
		case stdioComplete:
			elapsed := time.Since(start)
			if outputBytes != ipcTotalBytes || bodyFrames != ipcFrames {
				t.Fatalf("output bytes=%d Body frames=%d", outputBytes, bodyFrames)
			}
			if callbackChunks != ipcCallbackSize/ipcHTTPChunk {
				t.Fatalf("HTTP request/response chunks=%d", callbackChunks)
			}
			if credentialCalls != 2 {
				t.Fatalf("credential callback count=%d, want initial + stalled", credentialCalls)
			}
			if !bytes.Equal(hash.Sum(nil), wantHash) {
				t.Fatal("stream bytes differ from exact expected binary sequence")
			}
			_ = w.Close()
			reapStart := time.Now()
			select {
			case err = <-done:
				if err != nil {
					t.Fatalf("wait: %v", err)
				}
				if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
					cpu = timeNanos(usage.Utime) + timeNanos(usage.Stime)
				}
			case <-time.After(60 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("child reap timeout")
			}
			return stdioMeasure{first: first, timeNanos: elapsed.Nanoseconds(), reapNanos: time.Since(reapStart).Nanoseconds(), cpuNanos: cpu, rssBytes: rss, queueBytes: maxQueued, queueFrames: maxQueuedFrames, throughput: float64(outputBytes) / elapsed.Seconds()}
		default:
			t.Fatalf("unexpected frame kind %d", f.kind)
		}
	}
}

func timeNanos(tv syscall.Timeval) int64 {
	return tv.Sec*int64(time.Second) + tv.Usec*int64(time.Microsecond)
}

func TestM6IPCScenarioStdio(t *testing.T) {
	if ipcWireFrameMax != stdioFrameLimit {
		t.Fatalf("wire budget %d", ipcWireFrameMax)
	}
	var rows []stdioMeasure
	for i := 0; i < 11; i++ {
		m := runStdioScenario(t, nil, nil)
		if i > 0 {
			rows = append(rows, m)
		} else {
			t.Logf("stdio warmup: %+v", m)
		}
	}
	for i, m := range rows {
		t.Logf("stdio repetition=%d first_body_ms=%.3f completion_ms=%.3f reap_ms=%.3f MiB_s=%.2f cpu_ms=%.3f VmHWM_bytes=%d peak_queued_encoded_bytes=%d peak_queued_frames=%d", i+1, float64(m.first)/1e6, float64(m.timeNanos)/1e6, float64(m.reapNanos)/1e6, m.throughput/1048576, float64(m.cpuNanos)/1e6, m.rssBytes, m.queueBytes, m.queueFrames)
	}
	t.Logf("stdio first_body_median_ms=%.3f completion_median_ms=%.3f p95=%.3f; measured per-call peak queue=%d bytes/%d frames", median(rows, func(m stdioMeasure) int64 { return m.first })/1e6, median(rows, func(m stdioMeasure) int64 { return m.timeNanos })/1e6, p95(rows, func(m stdioMeasure) int64 { return m.timeNanos })/1e6, rows[len(rows)-1].queueBytes, rows[len(rows)-1].queueFrames)
	// Four calls hold output plus a reverse callback; cancellation must progress
	// before the three healthy streams' stalled HTTP services are released.
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	defer release()
	arrived := make(chan struct{}, 3)
	var wg sync.WaitGroup
	results := make(chan stdioMeasure, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- runStdioScenario(t, gate, arrived)
		}()
	}
	for i := 0; i < 3; i++ {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatal("saturation did not reach all three stalled callback/output queues")
		}
	}
	probe := beginStdioCancellation(t)
	if probe.queuedBytes != int64(stdioQueueLimit) || probe.queuedFrames != 2 {
		t.Fatalf("cancelled call queue bytes/frames=%d/%d", probe.queuedBytes, probe.queuedFrames)
	}
	cancelLatency := probe.cancelAndReap(t)
	if cancelLatency > int64(2*time.Second) {
		t.Fatalf("saturated cancel/reap %v exceeds 2s", time.Duration(cancelLatency))
	}
	release()
	workersDone := make(chan struct{})
	go func() { wg.Wait(); close(workersDone) }()
	select {
	case <-workersDone:
	case <-time.After(stdioRunDeadline):
		t.Fatal("saturated calls did not complete within the fixed 10-second bound")
	}
	close(results)
	var totalRSS int64
	var maxTime int64
	var totalQueue int64
	for m := range results {
		totalRSS += m.rssBytes
		totalQueue += m.queueBytes
		if m.timeNanos > maxTime {
			maxTime = m.timeNanos
		}
		if m.queueBytes != int64(stdioQueueLimit) || m.queueFrames != 2 {
			t.Errorf("stalled scenario queue bytes/frames=%d/%d", m.queueBytes, m.queueFrames)
		}
	}
	totalQueue += probe.queuedBytes
	wantSaturatedQueue := int64(4*stdioQueueLimit + 14) // Joint Stop control frame on the cancelled call.
	if totalQueue != wantSaturatedQueue {
		t.Fatalf("measured saturated queue=%d, want %d", totalQueue, wantSaturatedQueue)
	}
	if totalQueue > 21<<20 {
		t.Fatalf("saturated actual outstanding bytes %d exceed 21 MiB", totalQueue)
	}
	t.Logf("stdio saturation calls=4 healthy_stalled=3 cancelled=1 peak_queued_frames=9 peak_queued_encoded_bytes=%d per_healthy_call=%d cancelled_call=%d aggregate_child_VmHWM_sum=%d bytes healthy_slowest_completion_ms=%.3f saturated_cancel_to_reap_ms=%.3f", totalQueue, stdioQueueLimit, probe.queuedBytes, totalRSS+probe.rssBytes, float64(maxTime)/1e6, float64(cancelLatency)/1e6)
}

func median(a []stdioMeasure, f func(stdioMeasure) int64) float64 {
	x := make([]int64, len(a))
	for i, v := range a {
		x[i] = f(v)
	}
	sortInt64(x)
	return float64(x[len(x)/2])
}
func p95(a []stdioMeasure, f func(stdioMeasure) int64) float64 {
	x := make([]int64, len(a))
	for i, v := range a {
		x[i] = f(v)
	}
	sortInt64(x)
	return float64(x[(len(x)*95+99)/100-1])
}
func sortInt64(x []int64) { sort.Slice(x, func(i, j int) bool { return x[i] < x[j] }) }

func TestM6IPCStdioFrameFailures(t *testing.T) {
	badCorrelation := make([]byte, 13)
	binary.BigEndian.PutUint32(badCorrelation[:4], 9)
	badCorrelation[4] = stdioHead
	binary.BigEndian.PutUint64(badCorrelation[5:], stdioCallID+1)
	for name, data := range map[string][]byte{"truncated length": {0, 0}, "zero length": {0, 0, 0, 0}, "oversize": {0, 1, 0x50, 0x01}, "truncated payload": {0, 0, 0, 2, 1}, "stale correlation": badCorrelation} {
		t.Run(name, func(t *testing.T) {
			_, err := readStdio(bytes.NewReader(data))
			if err == nil {
				t.Fatal("invalid frame accepted")
			}
		})
	}
	for _, mode := range []string{"crash", "truncated", "oversize", "malformed"} {
		t.Run("child-"+mode, func(t *testing.T) {
			cmd, r, stdin, done := startStdioChild(t, mode)
			_ = stdin.Close()
			_, readErr := readStdio(r)
			if mode == "crash" {
				if readErr == nil {
					t.Fatal("crash produced a frame")
				}
			} else if readErr == nil {
				t.Fatal("invalid child frame accepted")
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("failed child hung")
			}
		})
	}
	for name, corrupt := range map[string]func([]byte){
		"header":      func(b []byte) { b[0] = 'x' },
		"opaque-body": func(b []byte) { b[len(b)-1] ^= 1 },
	} {
		t.Run("invalid-execute-"+name+"-rejected-before-output", func(t *testing.T) {
			cmd, r, w, done := startStdioChild(t, "scenario")
			bad := stdioRequestPayload()
			corrupt(bad)
			if err := writeStdio(w, stdioFrame{stdioExecute, bad}); err != nil {
				t.Fatal(err)
			}
			if frame, err := readStdio(r); err == nil {
				t.Fatalf("invalid Execute produced frame %+v", frame)
			}
			_ = w.Close()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("child accepted corrupted Execute request")
				}
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("invalid Execute hung")
			}
		})
	}
}

func TestM6IPCStdioDeadlineKillsAndReapsStalledChild(t *testing.T) {
	const deadline = 120 * time.Millisecond
	started := time.Now()
	cmd, r, w, done := startStdioChildWithDeadline(t, "stall", deadline)
	if err := writeStdio(w, stdioFrame{stdioExecute, stdioRequestPayload()}); err != nil {
		t.Fatal(err)
	}
	if _, err := readStdio(r); err == nil {
		t.Fatal("intentionally stalled child unexpectedly wrote output")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("deadline did not terminate stalled child")
		}
	case <-time.After(stdioReapDeadline):
		_ = cmd.Process.Kill()
		t.Fatal("deadline-killed child was not reaped")
	}
	elapsed := time.Since(started)
	if elapsed < deadline || elapsed > deadline+stdioReapDeadline {
		t.Fatalf("stall cleanup elapsed %v outside bounded deadline/reap window", elapsed)
	}
	if cmd.ProcessState == nil {
		t.Fatal("child exit was not reaped")
	}
	t.Logf("intentional pipe stall: deadline=%v killed/reaped after=%v", deadline, elapsed)
}

type stdioCancelProbe struct {
	cmd           *exec.Cmd
	w             io.WriteCloser
	done          <-chan error
	outputFrame   []byte
	callbackFrame []byte
	queuedBytes   int64
	queuedFrames  int
	rssBytes      int64
}

func beginStdioCancellation(t *testing.T) *stdioCancelProbe {
	t.Helper()
	cmd, r, w, done := startStdioChild(t, "cancel")
	if err := writeStdio(w, stdioFrame{stdioExecute, stdioRequestPayload()}); err != nil {
		t.Fatal(err)
	}
	if f, e := readStdio(r); e != nil || f.kind != stdioHead {
		t.Fatalf("Head %v %v", f, e)
	}
	first, e := readStdio(r)
	if e != nil || first.kind != stdioBody || len(first.body) != ipcFrameBytes {
		t.Fatalf("first Body %v %v", first, e)
	}
	if e = writeStdio(w, stdioFrame{stdioContinue, nil}); e != nil {
		t.Fatal(e)
	}
	body, e := readStdio(r)
	if e != nil || body.kind != stdioBody || len(body.body) != ipcFrameBytes || body.body[6] != 0 || body.body[7] != 0xff {
		t.Fatalf("queued Body %v %v", body, e)
	}
	httpFrame, e := readStdio(r)
	if e != nil || httpFrame.kind != stdioHTTP || len(httpFrame.body) != ipcHTTPChunk+ipcHeaderBytes || !bytes.Equal(httpFrame.body[:ipcHeaderBytes], bytes.Repeat([]byte{'h'}, ipcHeaderBytes)) || httpFrame.body[ipcHeaderBytes] != 0 || httpFrame.body[ipcHeaderBytes+1] != 0xff {
		t.Fatalf("blocked HTTP callback %v %v", httpFrame, e)
	}
	rss := procPeakRSS(cmd.Process.Pid)
	if rss == 0 {
		t.Fatal("cancelled child peak RSS unavailable while process active")
	}
	return &stdioCancelProbe{cmd: cmd, w: w, done: done, outputFrame: body.body, callbackFrame: httpFrame.body, queuedBytes: int64(len(body.body) + len(httpFrame.body) + 26), queuedFrames: 2, rssBytes: rss}
}

func (p *stdioCancelProbe) cancelAndReap(t *testing.T) int64 {
	t.Helper()
	start := time.Now()
	p.queuedBytes += 14 // Encoded Stop(mask=output|callback) control frame.
	p.queuedFrames++
	if p.queuedBytes > int64(21<<20)/4 {
		t.Fatalf("cancel frame queue %d exceeds worksheet per-call ceiling", p.queuedBytes)
	}
	if err := writeStdio(p.w, stdioFrame{stdioStop, []byte{3}}); err != nil {
		t.Fatal(err)
	}
	_ = p.w.Close()
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatal("child did not jointly release callback/output and reap on cancellation")
	}
	return time.Since(start).Nanoseconds()
}

func TestM6IPCStdioCancellationReapsChild(t *testing.T) {
	probe := beginStdioCancellation(t)
	latency := probe.cancelAndReap(t)
	t.Logf("joint-cancel request-to-exit/reap %.3fms pid=%d released=output+HTTP-callback queued_bytes=%d queued_frames=%d reaped=true", float64(latency)/1e6, probe.cmd.Process.Pid, probe.queuedBytes, probe.queuedFrames)
}

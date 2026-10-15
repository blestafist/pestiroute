//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/storage/sqlite"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
)

func TestM6ProtectedExternalE2E(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	child := filepath.Join(t.TempDir(), "responses-connector-child")
	build := exec.Command("go", "build", "-o", child, "./cmd/responses-connector-child")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build production child: %v\n%s", err, output)
	}

	for _, streaming := range []bool{false, true} {
		name := "json"
		if streaming {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			var upstream *fakeupstream.Server
			var release chan struct{}
			var upstreamWaiting chan struct{}
			var response []byte
			var completion []byte
			if streaming {
				response = append([]byte(": opaque "), 0xff, 0x00)
				response = append(response, []byte("\nevent: response.created\ndata: {\"type\":\"response.created\",\"future\":{\"opaque\":true}}\n\n")...)
				release, upstreamWaiting = make(chan struct{}), make(chan struct{}, 1)
				completion = []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\n\n")
				upstream = fakeupstream.New(fakeupstream.Response{
					Header: http.Header{"Content-Type": {"text/event-stream"}},
					Steps: []fakeupstream.Step{
						{Data: response},
						{Gate: release, Waiting: upstreamWaiting, Data: completion},
					},
				})
			} else {
				response = []byte(`{ "id":"resp_tool", "status":"completed", "output":[{"type":"function_call","call_id":"call_x","name":"lookup","arguments":"{\"raw\": [1,  2], \"unknown\":true}","future":{"keep":"bytewise","nul":"\u0000","invalid":"`)
				response = append(response, 0xff)
				response = append(response, []byte(`"}}],"usage":{"input_tokens":3,"output_tokens":4}}`)...)
				upstream = fakeupstream.New(fakeupstream.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: response})
			}
			defer upstream.Close()

			_, dbPath, keyPath := protectedFixture(t)
			t.Setenv("PESTIROUTE_M6_PARENT_ENV_CANARY", "not-for-child")
			c := fixtureProtectedConfig(dbPath, keyPath, "127.0.0.1:0")
			c.protected.Connectors[0].Settings.BaseURL = upstream.URL + "/v1/responses"
			if streaming {
				c.protected.Connectors[0].Settings.Capabilities = map[string]string{"llm.streaming": "supported"}
			}
			c.protected.Connectors[0].Process = &protectedProcess{Executable: child, WorkingDirectory: t.TempDir(), EnvironmentAllowlist: []string{}}
			prepared, err := prepareProtectedConfig(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			processDirsBefore := childProcessDirectories(t)
			issued, err := sqlite.NewVirtualKeys(prepared.runtimeDB).Create(context.Background(), sqlite.CreateVirtualKeyParams{PolicyID: "policy-id-a", PolicyRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			var ready, draining atomic.Bool
			parentChildren := directChildPIDs(t)
			handler, closeComponents, err := composeHandler(prepared, &ready, &draining, nil)
			if err != nil {
				t.Fatal(err)
			}
			ready.Store(true)
			childPID := newDirectChildPID(t, parentChildren)
			childExe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", childPID))
			if err != nil || filepath.Clean(childExe) != filepath.Clean(child) {
				t.Fatalf("launched child executable=%q err=%v; want production binary %q", childExe, err, child)
			}
			childEnv, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", childPID))
			if err != nil {
				t.Fatalf("read child environment: %v", err)
			}
			allowedEnv := map[string]bool{"PESTIROUTE_CONNECTOR_SOCKET": false, "PESTIROUTE_HOST_SERVICES_SOCKET": false}
			for _, entry := range bytes.Split(childEnv, []byte{0}) {
				if len(entry) == 0 {
					continue
				}
				key, _, _ := bytes.Cut(entry, []byte("="))
				if _, ok := allowedEnv[string(key)]; !ok {
					t.Fatalf("child inherited unexpected environment variable %q", key)
				}
				allowedEnv[string(key)] = true
			}
			for name, found := range allowedEnv {
				if !found {
					t.Fatalf("production child environment lacks required socket variable %s", name)
				}
			}
			childPGID, err := syscall.Getpgid(childPID)
			if err != nil || childPGID != childPID {
				t.Fatalf("production child PID=%d PGID=%d err=%v", childPID, childPGID, err)
			}
			gateway := httptest.NewServer(handler)
			defer func() {
				gateway.Close()
				if err := closeComponents(context.Background()); err != nil {
					t.Errorf("close protected gateway and child: %v", err)
				}
				_ = prepared.runtimeDB.Close()
				_ = prepared.processLock.Close()
			}()

			requestBody := []byte(`{"model":"model-a","input":[{"type":"function_call_output","call_id":"call_x","output":"opaque  bytes"}],"future":{"unknown":[1,  2]}}`)
			if streaming {
				requestBody = []byte(`{"model":"model-a","stream":true,"store":false,"input":"opaque"}`)
			}
			req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", bytes.NewReader(requestBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+issued.Secret)
			req.Header.Set("Content-Type", "application/json")
			client := &http.Client{Timeout: 15 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("gateway status=%d body=%s", resp.StatusCode, body)
			}
			capture := <-upstream.Requests
			if capture.Header.Get("Authorization") != "Bearer synthetic" {
				t.Fatalf("scoped credential not delivered to upstream: %q", capture.Header.Get("Authorization"))
			}
			if !bytes.Equal(capture.Body, requestBody) {
				t.Fatalf("request bytes changed across protected external path:\n got %q\nwant %q", capture.Body, requestBody)
			}
			if streaming {
				if resp.Header.Get("Content-Type") != "text/event-stream" {
					t.Fatalf("SSE content type = %q", resp.Header.Get("Content-Type"))
				}
				select {
				case <-upstreamWaiting:
				case <-time.After(5 * time.Second):
					t.Fatal("upstream did not hold completion gate")
				}
				first := make([]byte, len(response))
				if _, err := io.ReadFull(resp.Body, first); err != nil || !bytes.Equal(first, response) {
					t.Fatalf("first SSE chunk=%q err=%v", first, err)
				}
				select {
				case <-release:
					t.Fatal("upstream completed before gateway delivered first chunk")
				default:
				}
				close(release)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read gateway response: %v", err)
			}
			if streaming {
				want := append(append([]byte(nil), response...), completion...)
				if !bytes.Equal(append(response, got...), want) {
					t.Fatalf("SSE body did not preserve upstream bytes: %q", append(response, got...))
				}
			} else if !bytes.Equal(got, response) {
				t.Fatalf("JSON body changed:\n got %q\nwant %q", got, response)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatalf("close completed response: %v", err)
			}
			ledger := sqlite.NewLedger(prepared.runtimeDB)
			var attempt sqlite.AttemptUsage
			deadline := time.Now().Add(2 * time.Second)
			for {
				requests, queryErr := ledger.QueryRequests(context.Background(), sqlite.RequestFilter{Limit: 10})
				if queryErr != nil || len(requests) != 1 || len(requests[0].Attempts) != 1 {
					t.Fatalf("settled ledger requests=%+v err=%v", requests, queryErr)
				}
				attempt = requests[0].Attempts[0]
				if attempt.Attempt.State != "intent" || time.Now().After(deadline) {
					break
				}
				time.Sleep(time.Millisecond)
			}
			gateway.Close()
			if attempt.Attempt.State != "succeeded" || attempt.Usage == nil || attempt.Usage.InputTokens == nil || *attempt.Usage.InputTokens != 3 || attempt.Usage.OutputTokens == nil || *attempt.Usage.OutputTokens != 4 || attempt.Reservation.State != "settled" {
				t.Fatalf("external attempt not accurately settled: %+v", attempt)
			}
			if err := closeComponents(context.Background()); err != nil {
				t.Fatalf("shutdown protected gateway child: %v", err)
			}
			if err := syscall.Kill(-childPGID, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("production child process group %d remains after shutdown: %v", childPGID, err)
			}
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", childPID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("production child PID %d was not reaped: %v", childPID, err)
			}
			if after := childProcessDirectories(t); hasNewChildProcessDirectories(processDirsBefore, after) {
				t.Fatalf("external child socket directory leaked after shutdown: before=%v after=%v", processDirsBefore, after)
			}
		})
	}
}

func directChildPIDs(t *testing.T) map[int]struct{} {
	t.Helper()
	pid := os.Getpid()
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		t.Fatalf("read process task list: %v", err)
	}
	children := make(map[int]struct{})
	for _, task := range tasks {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", pid, task.Name()))
		if err != nil {
			t.Fatalf("read task %s child PIDs: %v", task.Name(), err)
		}
		for _, field := range strings.Fields(string(b)) {
			child, err := strconv.Atoi(field)
			if err != nil {
				t.Fatalf("invalid direct child PID %q: %v", field, err)
			}
			children[child] = struct{}{}
		}
	}
	return children
}

func newDirectChildPID(t *testing.T, before map[int]struct{}) int {
	t.Helper()
	after := directChildPIDs(t)
	var added []int
	for pid := range after {
		if _, existed := before[pid]; !existed {
			added = append(added, pid)
		}
	}
	if len(added) != 1 {
		t.Fatalf("protected composition launched %d direct child processes, want the production connector child", len(added))
	}
	return added[0]
}

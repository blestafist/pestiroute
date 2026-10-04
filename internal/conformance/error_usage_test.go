package conformance

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
	"github.com/blestafist/pestiroute/internal/testutil/fakeupstream"
	"github.com/blestafist/pestiroute/internal/testutil/scripted"
)

func TestConformanceErrorUsageRejectionsThroughDispatcher(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		category  core.ErrorCategory
		retryable bool
	}{
		{"bad-request", 400, core.CategoryInvalidRequest, false},
		{"unauthorized", 401, core.CategoryUnauthenticated, false},
		{"forbidden", 403, core.CategoryPermissionDenied, false},
		{"rate-limited", 429, core.CategoryRateLimited, true},
		{"unavailable", 503, core.CategoryUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(" {\"private\":\"opaque rejection ☃\"}\n")
			headers := http.Header{"Content-Type": {"application/json"}}
			if tc.retryable {
				headers.Set("Retry-After", "17")
			}
			fx, _ := nativeCustomFixture(t, fakeupstream.Response{Status: tc.status, Header: headers, Body: body})
			defer fx.close()
			if err := fx.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			d, attempts := dispatcherFor(t, fx, fx.services())
			response, gatewayErr := d.Execute(context.Background(), fx.request)
			if gatewayErr != nil {
				t.Fatalf("Execute: %v", gatewayErr)
			}
			frames, got, err := readFrames(response.Stream)
			if err != nil {
				t.Fatal(err)
			}
			assertRejectionFrames(t, frames, got, body, tc.category, tc.retryable, tc.retryable)
			assertOneAttempt(t, attempts, true, core.OutcomeFailed, tc.category)
			assertAttemptRetryMetadata(t, attempts, tc.retryable)
			if fx.callCount() != 1 {
				t.Fatalf("native upstream requests=%d, want exactly one", fx.callCount())
			}
			t.Run("anthropic-translation", func(t *testing.T) {
				translated := translationFixtureWithResponse(t, fakeupstream.Response{Status: tc.status, Header: headers, Body: body})
				defer translated.close()
				if err := translated.init(context.Background()); err != nil {
					t.Fatal(err)
				}
				d, attempts := dispatcherFor(t, translated, translated.services())
				_, gatewayErr := d.Execute(context.Background(), translated.request)
				if gatewayErr == nil || gatewayErr.Category != tc.category || gatewayErr.Retryable != tc.retryable || bytes.Contains([]byte(gatewayErr.Message), []byte("private")) {
					t.Fatalf("translation rejection=%+v; want category=%s retryable=%v without provider body", gatewayErr, tc.category, tc.retryable)
				}
				assertOneAttempt(t, attempts, false, core.OutcomeFailed, tc.category)
				assertAttemptRetryMetadata(t, attempts, tc.retryable)
				if translated.callCount() != 1 {
					t.Fatalf("translated upstream requests=%d, want one", translated.callCount())
				}
			})
		})
	}
}

func TestConformanceTranslationUsage(t *testing.T) {
	fx := translationFixture(t)
	defer fx.close()
	if err := fx.init(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, attempts := dispatcherFor(t, fx, fx.services())
	response, gatewayErr := d.Execute(context.Background(), fx.request)
	if gatewayErr != nil {
		t.Fatalf("Execute: %v", gatewayErr)
	}
	frames, body, err := readFrames(response.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if countFrames(frames, core.FrameComplete) != 1 || !bytes.Contains(body, []byte(`"type":"response.completed"`)) {
		t.Fatalf("translated terminal response missing: frames=%+v body=%q", frames, body)
	}
	complete := frames[len(frames)-1].Complete
	if complete == nil || complete.Usage == nil {
		t.Fatalf("translated completion missing provider usage: %+v", frames)
	}
	assertOneAttempt(t, attempts, true, core.OutcomeSucceeded, "")
	if !(*attempts)[0].HasUsage {
		t.Fatal("dispatcher did not retain translated provider usage")
	}
	usage := (*attempts)[0].Usage
	if usage.Source != core.UsageProvider || usage.Completeness != core.UsageComplete || usage.InputTokens == nil || *usage.InputTokens != 2 || usage.OutputTokens == nil || *usage.OutputTokens != 3 {
		t.Fatalf("translated provider usage was not retained: %+v frames=%+v body=%q", usage, frames, body)
	}
}

func TestConformanceScriptedRejectionsThroughDispatcher(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		category  core.ErrorCategory
		retryable bool
	}{
		{"bad-request", 400, core.CategoryInvalidRequest, false},
		{"unauthorized", 401, core.CategoryUnauthenticated, false},
		{"forbidden", 403, core.CategoryPermissionDenied, false},
		{"rate-limited", 429, core.CategoryRateLimited, true},
		{"unavailable", 503, core.CategoryUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(" {\"private\":\"scripted opaque bytes\"}\n")
			ge := &core.GatewayError{Code: "upstream_rejection", Category: tc.category, Retryable: tc.retryable, RetryDisposition: core.RetryUnknown, Message: "Upstream rejected request"}
			if tc.retryable {
				delay := 17 * time.Second
				ge.RetryAfter = &delay
			}
			status := tc.status
			fx := newScriptedCustomFixture(t, rejectedSteps(status, ge, body))
			defer fx.close()
			if err := fx.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			d, attempts := dispatcherFor(t, fx, fx.services())
			response, gatewayErr := d.Execute(context.Background(), fx.request)
			if gatewayErr != nil {
				t.Fatalf("Execute: %v", gatewayErr)
			}
			frames, got, err := readFrames(response.Stream)
			if err != nil {
				t.Fatal(err)
			}
			assertRejectionFrames(t, frames, got, body, tc.category, tc.retryable, tc.retryable)
			assertOneAttempt(t, attempts, true, core.OutcomeFailed, tc.category)
			assertAttemptRetryMetadata(t, attempts, tc.retryable)
			if fx.callCount() != 1 {
				t.Fatalf("scripted Execute count=%d, want exactly one", fx.callCount())
			}
		})
	}
}

func TestConformanceDeclaredFailureAndUsageThroughDispatcher(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   []byte
		sse    bool
		failed bool
	}{
		{name: "completed-usage", body: []byte(`{"status":"completed","usage":{"input_tokens":12,"output_tokens":5}}`)},
		{name: "completed-explicit-zero-details", body: []byte(`{"status":"completed","usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`)},
		{name: "missing-usage-counter-is-unknown", body: []byte(`{"status":"completed","usage":{"input_tokens":12}}`)},
		{name: "failed-json-retains-usage", body: []byte(`{"status":"failed","error":{"message":"private"},"usage":{"input_tokens":12,"output_tokens":5,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":2}}}`), failed: true},
		{name: "failed-sse-retains-usage", body: []byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":4},\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n"), sse: true, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contentType := "application/json"
			if tc.sse {
				contentType = "text/event-stream"
			}
			fx, _ := nativeCustomFixture(t, fakeupstream.Response{Header: http.Header{"Content-Type": {contentType}}, Body: tc.body})
			defer fx.close()
			if err := fx.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tc.sse {
				streaming := true
				fx.request.Metadata.Streaming = &streaming
			}
			d, attempts := dispatcherFor(t, fx, fx.services())
			response, gatewayErr := d.Execute(context.Background(), fx.request)
			if gatewayErr != nil {
				t.Fatalf("Execute: %v", gatewayErr)
			}
			frames, got, err := readFrames(response.Stream)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.body) {
				t.Fatalf("native response bytes changed: %q want %q", got, tc.body)
			}
			wantOutcome := core.OutcomeSucceeded
			wantCategory := core.ErrorCategory("")
			if tc.failed {
				wantOutcome, wantCategory = core.OutcomeFailed, core.CategoryUnavailable
			}
			assertOneAttempt(t, attempts, true, wantOutcome, wantCategory)
			if fx.callCount() != 1 {
				t.Fatalf("native upstream requests=%d, want exactly one", fx.callCount())
			}
			if len(frames) == 0 || frames[len(frames)-1].Type != core.FrameComplete {
				t.Fatalf("missing terminal frame: %+v", frames)
			}
			assertUsage(t, (*attempts)[0].Usage, (*attempts)[0].HasUsage, tc.name)
		})
	}
}

func TestConformanceScriptedDeclaredFailureThroughDispatcher(t *testing.T) {
	body := []byte(`{"status":"failed","opaque":true}`)
	ge := &core.GatewayError{Code: "provider_failure", Category: core.CategoryUnavailable, Message: "Upstream reported a failure"}
	fx := newScriptedCustomFixture(t, rejectedSteps(http.StatusOK, ge, body))
	defer fx.close()
	if err := fx.init(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, attempts := dispatcherFor(t, fx, fx.services())
	response, gatewayErr := d.Execute(context.Background(), fx.request)
	if gatewayErr != nil {
		t.Fatalf("Execute: %v", gatewayErr)
	}
	frames, got, err := readFrames(response.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) || len(frames) != 3 || frames[0].Head.HTTPStatus == nil || *frames[0].Head.HTTPStatus != http.StatusOK {
		t.Fatalf("scripted 200 failure payload/status changed: body=%q frames=%+v", got, frames)
	}
	assertOneAttempt(t, attempts, true, core.OutcomeFailed, core.CategoryUnavailable)
	if fx.callCount() != 1 {
		t.Fatalf("scripted Execute count=%d, want exactly one", fx.callCount())
	}
}

func TestConformanceScriptedPreHeadErrorThroughDispatcher(t *testing.T) {
	prehead := &core.GatewayError{Code: "credential_rejected", Category: core.CategoryUnauthenticated, Message: "Credential rejected"}
	fx := newScriptedExecuteErrorFixture(t, prehead)
	defer fx.close()
	if err := fx.init(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, attempts := dispatcherFor(t, fx, fx.services())
	response, gatewayErr := d.Execute(context.Background(), fx.request)
	if gatewayErr == nil || gatewayErr.Category != core.CategoryUnauthenticated || response.Stream != nil {
		t.Fatalf("pre-Head scripted result: response=%+v error=%+v", response, gatewayErr)
	}
	assertOneAttempt(t, attempts, false, core.OutcomeFailed, core.CategoryUnauthenticated)
	if fx.callCount() != 1 {
		t.Fatalf("scripted Execute count=%d, want exactly one", fx.callCount())
	}
}

func TestConformanceScriptedInterruptedUsageThroughDispatcher(t *testing.T) {
	input := int64(7)
	partial := &core.UsageReport{InputTokens: &input, Source: core.UsageProvider, Completeness: core.UsagePartial}
	ge := &core.GatewayError{Code: "interrupted", Category: core.CategoryUnavailable, Message: "Upstream response was interrupted"}
	fx := newScriptedCustomFixture(t, []scripted.Step{
		{Frame: headFrame()},
		{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: []byte("observed-prefix")}}},
		{Frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: ge, Usage: partial}}},
	})
	defer fx.close()
	if err := fx.init(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, attempts := dispatcherFor(t, fx, fx.services())
	response, gatewayErr := d.Execute(context.Background(), fx.request)
	if gatewayErr != nil {
		t.Fatalf("Execute: %v", gatewayErr)
	}
	frames, body, err := readFrames(response.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "observed-prefix" || len(frames) != 3 || frames[2].Complete.Outcome != core.OutcomeIncomplete {
		t.Fatalf("interrupted script lifecycle: body=%q frames=%+v", body, frames)
	}
	assertOneAttempt(t, attempts, true, core.OutcomeIncomplete, core.CategoryUnavailable)
	got := (*attempts)[0]
	if !got.HasUsage || got.Usage.Source != core.UsageProvider || got.Usage.Completeness != core.UsagePartial || got.Usage.InputTokens == nil || *got.Usage.InputTokens != input || got.Usage.OutputTokens != nil {
		t.Fatalf("observed partial usage was lost or fabricated: %+v", got)
	}
}

func TestConformanceNoReplayForSafePreHeadDisposition(t *testing.T) {
	calls := 0
	var attempts []core.AttemptResult
	d := &core.Dispatcher{
		AccountID: account,
		Target: conformanceTarget(func(context.Context, core.ExecutionRequest, core.AttemptScope) (core.ExecutionResponse, *core.GatewayError) {
			calls++
			return core.ExecutionResponse{}, &core.GatewayError{Code: "limited", Category: core.CategoryRateLimited, Retryable: true, RetryDisposition: core.RetrySafe, Message: "Try again later"}
		}),
		Finalize: func(result core.AttemptResult) { attempts = append(attempts, result) },
	}
	response, gatewayErr := d.Execute(context.Background(), core.ExecutionRequest{Model: model, Payload: core.RawPayload{Protocol: protocol}})
	if gatewayErr == nil || response.Stream != nil || calls != 1 || len(attempts) != 1 {
		t.Fatalf("safe retry disposition caused hidden replay: response=%+v error=%+v calls=%d attempts=%+v", response, gatewayErr, calls, attempts)
	}
	if attempts[0].Committed || attempts[0].Outcome != core.OutcomeFailed || attempts[0].Error == nil || attempts[0].Error.RetryDisposition != core.RetrySafe {
		t.Fatalf("safe pre-Head attempt record: %+v", attempts[0])
	}
}

func TestConformanceNativePreHeadAndPostCommitNoReplay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response fakeupstream.Response
		services func(fixture) core.InvocationServices
	}{
		{name: "missing-credential", response: fakeupstream.Response{}, services: func(fixture) core.InvocationServices { return core.InvocationServices{} }},
		{name: "transport-drop-before-headers", response: fakeupstream.Response{Drop: true}, services: func(fx fixture) core.InvocationServices { return fx.services() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx, _ := nativeCustomFixture(t, tc.response)
			defer fx.close()
			if err := fx.init(context.Background()); err != nil {
				t.Fatal(err)
			}
			d, attempts := dispatcherFor(t, fx, tc.services(fx))
			response, gatewayErr := d.Execute(context.Background(), fx.request)
			if gatewayErr == nil || response.Stream != nil {
				t.Fatalf("pre-Head failure returned stream/success: response=%+v error=%+v", response, gatewayErr)
			}
			if gatewayErr.RetryDisposition == core.RetrySafe {
				t.Fatalf("pre-Head native failure was declared replay-safe: %+v", gatewayErr)
			}
			assertOneAttempt(t, attempts, false, core.OutcomeFailed, gatewayErr.Category)
			if fx.callCount() != boolCount(tc.name == "transport-drop-before-headers") {
				t.Fatalf("upstream request count=%d", fx.callCount())
			}
		})
	}

	t.Run("drop-after-commit", func(t *testing.T) {
		prefix := []byte("opaque prefix")
		fx, _ := nativeCustomFixture(t, fakeupstream.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Steps: []fakeupstream.Step{{Data: prefix, Drop: true}}})
		defer fx.close()
		if err := fx.init(context.Background()); err != nil {
			t.Fatal(err)
		}
		streaming := true
		fx.request.Metadata.Streaming = &streaming
		d, attempts := dispatcherFor(t, fx, fx.services())
		response, gatewayErr := d.Execute(context.Background(), fx.request)
		if gatewayErr != nil {
			t.Fatalf("Execute: %v", gatewayErr)
		}
		frames, got, err := readFrames(response.Stream)
		if err != nil {
			t.Fatalf("dropped stream read: %v", err)
		}
		if !bytes.Equal(got, prefix) || countFrames(frames, core.FrameHead) != 1 || countFrames(frames, core.FrameComplete) != 1 || frames[len(frames)-1].Complete.Outcome != core.OutcomeIncomplete || frames[len(frames)-1].Complete.Error == nil {
			t.Fatalf("post-commit delivery changed: body=%q frames=%+v", got, frames)
		}
		assertOneAttempt(t, attempts, true, core.OutcomeIncomplete, core.CategoryUnavailable)
		if (*attempts)[0].Error.RetryDisposition == core.RetrySafe || fx.callCount() != 1 {
			t.Fatalf("ambiguous drop replay metadata/requests: error=%+v requests=%d", (*attempts)[0].Error, fx.callCount())
		}
	})
}

func dispatcherFor(t *testing.T, fx fixture, services core.InvocationServices) (*core.Dispatcher, *[]core.AttemptResult) {
	t.Helper()
	var attempts []core.AttemptResult
	if fx.translated {
		d := fx.dispatcher
		d.Finalize = func(result core.AttemptResult) { attempts = append(attempts, result) }
		return d, &attempts
	}
	d := &core.Dispatcher{AccountID: account, Finalize: func(result core.AttemptResult) { attempts = append(attempts, result) }}
	d.Target = conformanceTarget(func(ctx context.Context, in core.ExecutionRequest, scope core.AttemptScope) (core.ExecutionResponse, *core.GatewayError) {
		// Dispatcher assigns a fresh ID; scripted scripts are intentionally keyed
		// to this test fixture's fixed ID. Map only the fixture lookup key.
		if _, ok := fx.connector.(*scripted.Connector); ok {
			in.ID = request
		}
		return fx.connector.Execute(ctx, in, scope, services)
	})
	return d, &attempts
}

func newScriptedExecuteErrorFixture(t *testing.T, gatewayErr *core.GatewayError) fixture {
	t.Helper()
	descriptor := core.Descriptor{
		ID: "conformance.scripted.prehead", Kind: core.ComponentConnector, ImplementationVersion: "test",
		APIVersions: []core.APIVersion{{Major: 1}}, Protocols: []string{protocol},
		Operations: []string{"execute"}, ConnectorType: "test", AuthMethods: []string{"bearer"},
	}
	connector := scripted.New(descriptor, nil, scripted.Script{ID: request, ExecuteError: gatewayErr})
	t.Setenv("PESTIROUTE_CONFORMANCE_TOKEN", "synthetic-token")
	scope := core.AttemptScope{AccountID: account, Mode: "native"}
	return fixture{
		connector: connector,
		init:      func(ctx context.Context) error { return connector.Init(ctx, core.ComponentConfig{}) },
		request:   core.ExecutionRequest{ID: request, Model: model, Payload: core.RawPayload{Protocol: protocol, ContentType: "application/json", Body: []byte(`{"model":"gpt-5.4-mini","input":"test"}`)}},
		scope:     scope,
		services: func() core.InvocationServices {
			return core.NewEnvironmentServices(map[string]map[string]string{account: {"bearer": "PESTIROUTE_CONFORMANCE_TOKEN"}}, nil, nil).ForAttempt(scope)
		},
		callCount: func() int64 { return int64(connector.CallCount()) },
		close: func() {
			if err := connector.Close(context.Background()); err != nil {
				t.Errorf("close scripted connector: %v", err)
			}
		},
	}
}

func rejectedSteps(status int, ge *core.GatewayError, body []byte) []scripted.Step {
	return []scripted.Step{
		{Frame: core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: protocol, HTTPStatus: &status, Error: ge}}},
		{Frame: core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: body}}},
		{Frame: core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeFailed, Error: ge}}},
	}
}

func assertRejectionFrames(t *testing.T, frames []core.StreamFrame, got, want []byte, category core.ErrorCategory, retryable, hasRetryAfter bool) {
	t.Helper()
	if !bytes.Equal(got, want) || len(frames) != 3 || frames[0].Type != core.FrameHead || frames[0].Head.Error == nil || frames[0].Head.Error.Category != category || frames[0].Head.Error.Retryable != retryable {
		t.Fatalf("rejection body/head mismatch: body=%q frames=%+v", got, frames)
	}
	ge := frames[0].Head.Error
	if ge.RetryDisposition != core.RetryUnknown || ge.Message == "" || ge.Provider != "" || ge.OriginalError != "" {
		t.Fatalf("unsafe or unsanitized rejection metadata: %+v", ge)
	}
	if hasRetryAfter {
		if ge.RetryAfter == nil || *ge.RetryAfter != 17*time.Second {
			t.Fatalf("Retry-After = %v, want 17s", ge.RetryAfter)
		}
	} else if ge.RetryAfter != nil {
		t.Fatalf("unexpected Retry-After: %v", ge.RetryAfter)
	}
	if frames[2].Type != core.FrameComplete || frames[2].Complete.Outcome != core.OutcomeFailed || frames[2].Complete.Error == nil || frames[2].Complete.Error.Category != category {
		t.Fatalf("rejection completion: %+v", frames[2])
	}
}

func assertOneAttempt(t *testing.T, attempts *[]core.AttemptResult, committed bool, outcome core.Outcome, category core.ErrorCategory) {
	t.Helper()
	if len(*attempts) != 1 {
		t.Fatalf("attempt records=%d, want exactly one: %+v", len(*attempts), *attempts)
	}
	got := (*attempts)[0]
	if got.Committed != committed || got.Outcome != outcome {
		t.Fatalf("attempt commit/outcome: %+v", got)
	}
	if category == "" {
		if got.Error != nil {
			t.Fatalf("unexpected attempt error: %+v", got.Error)
		}
		return
	}
	if got.Error == nil || got.Error.Category != category || got.Error.Message == "" || got.Error.Provider != "" || got.Error.OriginalError != "" {
		t.Fatalf("attempt error is absent, misclassified, or unsafe: %+v", got.Error)
	}
}

func assertAttemptRetryMetadata(t *testing.T, attempts *[]core.AttemptResult, retryable bool) {
	t.Helper()
	ge := (*attempts)[0].Error
	if ge == nil || ge.Retryable != retryable || ge.RetryDisposition != core.RetryUnknown {
		t.Fatalf("attempt retry metadata: %+v", ge)
	}
	if retryable && (ge.RetryAfter == nil || *ge.RetryAfter != 17*time.Second) {
		t.Fatalf("attempt Retry-After = %v, want 17s", ge.RetryAfter)
	}
	if !retryable && ge.RetryAfter != nil {
		t.Fatalf("unexpected attempt Retry-After: %v", ge.RetryAfter)
	}
}

func assertUsage(t *testing.T, usage core.UsageReport, hasUsage bool, scenario string) {
	t.Helper()
	if scenario == "completed-usage" {
		if !hasUsage || usage.Source != core.UsageProvider || usage.Completeness != core.UsageComplete || usage.InputTokens == nil || *usage.InputTokens != 12 || usage.OutputTokens == nil || *usage.OutputTokens != 5 || usage.CachedTokens != nil || usage.ReasoningTokens != nil {
			t.Fatalf("totals/detail absence: %+v hasUsage=%v", usage, hasUsage)
		}
	}
	if scenario == "completed-explicit-zero-details" {
		if !hasUsage || usage.Source != core.UsageProvider || usage.Completeness != core.UsageComplete || usage.InputTokens == nil || *usage.InputTokens != 0 || usage.OutputTokens == nil || *usage.OutputTokens != 0 || usage.CachedTokens == nil || *usage.CachedTokens != 0 || usage.ReasoningTokens == nil || *usage.ReasoningTokens != 0 {
			t.Fatalf("explicit zeros: %+v hasUsage=%v", usage, hasUsage)
		}
	}
	if scenario == "missing-usage-counter-is-unknown" {
		if !hasUsage || usage.Source != core.UsageUnknown || usage.Completeness != core.UsageUnknownCompleteness || usage.InputTokens != nil || usage.OutputTokens != nil {
			t.Fatalf("missing counter became zero/known: %+v hasUsage=%v", usage, hasUsage)
		}
	}
	if scenario == "failed-json-retains-usage" || scenario == "failed-sse-retains-usage" {
		if !hasUsage || usage.Source != core.UsageProvider || usage.Completeness != core.UsageComplete || usage.InputTokens == nil || *usage.InputTokens != 12 || usage.OutputTokens == nil || *usage.OutputTokens != 5 || usage.CachedTokens == nil || *usage.CachedTokens != 4 || usage.ReasoningTokens == nil || *usage.ReasoningTokens != 2 {
			t.Fatalf("failed attempt lost provider usage: %+v hasUsage=%v", usage, hasUsage)
		}
	}
}

func readFrames(stream core.Stream) ([]core.StreamFrame, []byte, error) {
	var frames []core.StreamFrame
	var body []byte
	for {
		frame, err := stream.Next(context.Background())
		if err == io.EOF {
			return frames, body, nil
		}
		if err != nil {
			return frames, body, err
		}
		frames = append(frames, frame)
		if frame.Type == core.FrameBody {
			body = append(body, frame.Body.Data...)
		}
	}
}

func countFrames(frames []core.StreamFrame, kind core.FrameType) int {
	count := 0
	for _, frame := range frames {
		if frame.Type == kind {
			count++
		}
	}
	return count
}

func boolCount(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

type conformanceTarget func(context.Context, core.ExecutionRequest, core.AttemptScope) (core.ExecutionResponse, *core.GatewayError)

func (f conformanceTarget) Execute(ctx context.Context, request core.ExecutionRequest, scope core.AttemptScope) (core.ExecutionResponse, *core.GatewayError) {
	return f(ctx, request, scope)
}

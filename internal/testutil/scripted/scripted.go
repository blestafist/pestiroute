// Package scripted provides deterministic, test-only core Connector scripts.
package scripted

import (
	"context"
	"io"
	"slices"
	"sync"

	"github.com/blestafist/pestiroute/internal/core"
)

// Step releases one frame when Gate is closed. A nil gate releases immediately.
type Step struct {
	Frame   core.StreamFrame
	Err     error // Returned instead of Frame, including after Head.
	Gate    <-chan struct{}
	Waiting chan<- struct{} // Notified once Next reaches this gate.
}

// Script is selected by ExecutionRequest.ID; each ID can execute only once.
type Script struct {
	ID           string
	ExecuteError *core.GatewayError // Returned before a stream is created.
	Steps        []Step
}

// Call is an immutable snapshot of one Execute input.
type Call struct {
	Request core.ExecutionRequest
	Scope   core.AttemptScope
}

type Connector struct {
	mu            sync.Mutex
	descriptor    core.Descriptor
	capabilities  map[core.CapabilityScope]core.CapabilityResult
	scripts       map[string][]Step
	executeErrors map[string]*core.GatewayError
	calls         []Call
	streams       map[*stream]struct{}
	init          bool
	closed        bool
	closeOnce     sync.Once
	closeCount    int
}

func New(descriptor core.Descriptor, capabilities map[core.CapabilityScope]core.CapabilityResult, scripts ...Script) *Connector {
	c := &Connector{
		descriptor:    descriptor.Clone(),
		capabilities:  make(map[core.CapabilityScope]core.CapabilityResult, len(capabilities)),
		scripts:       make(map[string][]Step, len(scripts)),
		executeErrors: make(map[string]*core.GatewayError, len(scripts)),
		streams:       make(map[*stream]struct{}),
	}
	for scope, result := range capabilities {
		c.capabilities[scope] = result.Clone()
	}
	for _, script := range scripts {
		c.scripts[script.ID] = cloneSteps(script.Steps)
		if script.ExecuteError != nil {
			c.executeErrors[script.ID] = cloneGatewayError(script.ExecuteError)
		}
	}
	return c
}

func (c *Connector) Descriptor() core.Descriptor { return c.descriptor.Clone() }

func (c *Connector) Init(ctx context.Context, _ core.ComponentConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return context.Canceled
	}
	c.init = true
	return nil
}

func (c *Connector) Health(context.Context) core.Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.init {
		return core.Health{State: core.HealthUnavailable}
	}
	return core.Health{State: core.HealthReady}
}

func (c *Connector) Capabilities(_ context.Context, scope core.CapabilityScope) core.CapabilityResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capabilities[scope].Clone()
}

func (c *Connector) Close(context.Context) error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.closeCount++
		active := make([]*stream, 0, len(c.streams))
		for s := range c.streams {
			active = append(active, s)
		}
		c.mu.Unlock()
		for _, s := range active {
			_ = s.Close()
		}
	})
	return nil
}

func (c *Connector) Execute(ctx context.Context, request core.ExecutionRequest, scope core.AttemptScope, services core.InvocationServices) (core.ExecutionResponse, *core.GatewayError) {
	if err := ctx.Err(); err != nil {
		return core.ExecutionResponse{}, cancelled(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.init {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "unavailable", Category: core.CategoryUnavailable, Message: "Scripted connector unavailable"}
	}
	if !declaresProtocol(c.descriptor, request.Payload.Protocol) {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "unsupported_protocol", Category: core.CategoryUnsupportedFeature, Message: "Scripted connector does not declare the request protocol"}
	}
	if len(c.capabilities) != 0 {
		if _, ok := c.capabilities[core.CapabilityScope{
			Protocol: request.Payload.Protocol, Mode: scope.Mode, Model: request.Model, AccountID: scope.AccountID,
		}]; !ok {
			return core.ExecutionResponse{}, &core.GatewayError{Code: "scope_mismatch", Category: core.CategoryPermissionDenied, Message: "Scripted connector does not declare the selected scope"}
		}
	}
	if declaresAuthMethod(c.descriptor, "bearer") {
		if services.Credentials == nil {
			return core.ExecutionResponse{}, &core.GatewayError{Code: "credential_unavailable", Category: core.CategoryUnauthenticated, Message: "Selected credential is unavailable"}
		}
		credential, err := services.Credentials.Get(ctx, "bearer")
		if err != nil || len(credential) == 0 {
			return core.ExecutionResponse{}, &core.GatewayError{Code: "credential_unavailable", Category: core.CategoryUnauthenticated, Message: "Selected credential is unavailable"}
		}
	}
	if gatewayErr := c.executeErrors[request.ID]; gatewayErr != nil {
		delete(c.executeErrors, request.ID)
		delete(c.scripts, request.ID)
		c.calls = append(c.calls, Call{Request: cloneRequest(request), Scope: scope})
		return core.ExecutionResponse{}, cloneGatewayError(gatewayErr)
	}
	steps, ok := c.scripts[request.ID]
	if !ok {
		return core.ExecutionResponse{}, &core.GatewayError{Code: "script_not_found", Category: core.CategoryInvalidRequest, Message: "No execution script"}
	}
	delete(c.scripts, request.ID)
	c.calls = append(c.calls, Call{Request: cloneRequest(request), Scope: scope})
	s := &stream{steps: steps, done: make(chan struct{}), owner: c}
	c.streams[s] = struct{}{}
	return core.ExecutionResponse{Stream: s}, nil
}

func declaresProtocol(descriptor core.Descriptor, protocol string) bool {
	return slices.Contains(descriptor.Protocols, protocol)
}

func declaresAuthMethod(descriptor core.Descriptor, method string) bool {
	return slices.Contains(descriptor.AuthMethods, method)
}

// Models, EstimateUsage and Authenticate intentionally report unsupported;
// scripts test execution, not fabricated provider support.
func (*Connector) Models(context.Context, core.ModelQuery, core.InvocationServices) (core.ModelsResult, *core.GatewayError) {
	return core.ModelsResult{}, nil
}
func (*Connector) EstimateUsage(context.Context, core.UsageQuery, core.InvocationServices) (core.EstimateResult, *core.GatewayError) {
	return core.EstimateResult{}, nil
}
func (*Connector) Authenticate(context.Context, core.AuthRequest, core.InvocationServices) (core.AuthResult, *core.GatewayError) {
	return core.AuthResult{}, nil
}

func (c *Connector) Calls() []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Call, len(c.calls))
	for i, call := range c.calls {
		out[i] = Call{Request: cloneRequest(call.Request), Scope: call.Scope}
	}
	return out
}

func (c *Connector) CallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *Connector) CloseCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeCount
}

type stream struct {
	mu    sync.Mutex
	steps []Step
	next  int
	done  chan struct{}
	once  sync.Once
	owner *Connector
}

func (s *stream) Next(ctx context.Context) (core.StreamFrame, error) {
	s.mu.Lock()
	if s.next == len(s.steps) {
		s.mu.Unlock()
		return core.StreamFrame{}, io.EOF
	}
	step := s.steps[s.next]
	s.mu.Unlock()
	if step.Gate != nil {
		if step.Waiting != nil {
			select {
			case step.Waiting <- struct{}{}:
			case <-ctx.Done():
				return core.StreamFrame{}, ctx.Err()
			case <-s.done:
				return core.StreamFrame{}, context.Canceled
			}
		}
		select {
		case <-step.Gate:
		case <-ctx.Done():
			return core.StreamFrame{}, ctx.Err()
		case <-s.done:
			return core.StreamFrame{}, context.Canceled
		}
	} else if err := ctx.Err(); err != nil {
		return core.StreamFrame{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return core.StreamFrame{}, context.Canceled
	default:
	}
	if s.next >= len(s.steps) || s.steps[s.next].Gate != step.Gate {
		return core.StreamFrame{}, context.Canceled
	}
	s.next++
	if step.Err != nil {
		return core.StreamFrame{}, step.Err
	}
	return cloneFrame(step.Frame), nil
}

func (s *stream) Close() error {
	s.once.Do(func() {
		close(s.done)
		s.owner.mu.Lock()
		delete(s.owner.streams, s)
		s.owner.mu.Unlock()
	})
	return nil
}

func cloneSteps(steps []Step) []Step {
	out := make([]Step, len(steps))
	for i, step := range steps {
		out[i] = Step{Frame: cloneFrame(step.Frame), Err: step.Err, Gate: step.Gate, Waiting: step.Waiting}
	}
	return out
}

func cloneFrame(frame core.StreamFrame) core.StreamFrame {
	if frame.Head != nil {
		head := *frame.Head
		head.Headers = cloneHeaders(head.Headers)
		if head.HTTPStatus != nil {
			status := *head.HTTPStatus
			head.HTTPStatus = &status
		}
		if head.Error != nil {
			head.Error = cloneGatewayError(head.Error)
		}
		frame.Head = &head
	}
	if frame.Body != nil {
		body := *frame.Body
		body.Data = append([]byte(nil), body.Data...)
		frame.Body = &body
	}
	if frame.Complete != nil {
		complete := *frame.Complete
		if complete.Error != nil {
			complete.Error = cloneGatewayError(complete.Error)
		}
		if complete.Usage != nil {
			usage := *complete.Usage
			usage.InputTokens = cloneInt64(usage.InputTokens)
			usage.OutputTokens = cloneInt64(usage.OutputTokens)
			usage.ReasoningTokens = cloneInt64(usage.ReasoningTokens)
			usage.CachedTokens = cloneInt64(usage.CachedTokens)
			complete.Usage = &usage
		}
		frame.Complete = &complete
	}
	return frame
}

func cloneRequest(request core.ExecutionRequest) core.ExecutionRequest {
	request.Capabilities = cloneCapabilities(request.Capabilities)
	request.Metadata.Headers = cloneHeaders(request.Metadata.Headers)
	request.Metadata.Extensions = cloneExtensions(request.Metadata.Extensions)
	if request.Metadata.Streaming != nil {
		streaming := *request.Metadata.Streaming
		request.Metadata.Streaming = &streaming
	}
	request.Payload.Body = append([]byte(nil), request.Payload.Body...)
	return request
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneGatewayError(value *core.GatewayError) *core.GatewayError {
	cloned := *value
	if value.RetryAfter != nil {
		duration := *value.RetryAfter
		cloned.RetryAfter = &duration
	}
	return &cloned
}

func cloneExtensions(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		switch value := value.(type) {
		case []byte:
			out[key] = append([]byte(nil), value...)
		case []string:
			out[key] = append([]string(nil), value...)
		case map[string]string:
			copy := make(map[string]string, len(value))
			for k, v := range value {
				copy[k] = v
			}
			out[key] = copy
		case map[string]any:
			out[key] = cloneExtensions(value)
		case []any:
			copy := make([]any, len(value))
			for i, item := range value {
				copy[i] = cloneExtensionValue(item)
			}
			out[key] = copy
		default:
			out[key] = value
		}
	}
	return out
}

func cloneExtensionValue(value any) any {
	if extensions, ok := value.(map[string]any); ok {
		return cloneExtensions(extensions)
	}
	if bytes, ok := value.([]byte); ok {
		return append([]byte(nil), bytes...)
	}
	if values, ok := value.([]any); ok {
		copy := make([]any, len(values))
		for i, item := range values {
			copy[i] = cloneExtensionValue(item)
		}
		return copy
	}
	return value
}

func cloneCapabilities(in map[core.Capability]struct{}) map[core.Capability]struct{} {
	out := make(map[core.Capability]struct{}, len(in))
	for key := range in {
		out[key] = struct{}{}
	}
	return out
}

func cloneHeaders(in map[string][]string) map[string][]string {
	if in == nil {
		return nil
	}
	out := make(map[string][]string, len(in))
	for key, values := range in {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func cancelled(err error) *core.GatewayError {
	return &core.GatewayError{Code: "cancelled", Category: core.CategoryCancelled, Message: "Scripted execution cancelled", OriginalError: err.Error()}
}

var _ core.Connector = (*Connector)(nil)
var _ core.Stream = (*stream)(nil)

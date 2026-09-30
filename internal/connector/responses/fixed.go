package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const protocol = "openai.responses.v1"
const observationLimit = 1 << 20
const chunkSize = 32 << 10

// ExecuteFixedJSON performs one native, non-streaming attempt. The caller owns
// the returned stream and must close it even when abandoning it before EOF.
func (t *Transport) ExecuteFixedJSON(ctx context.Context, in core.ExecutionRequest) (core.ExecutionResponse, *core.GatewayError) {
	if in.Payload.Protocol != protocol {
		return core.ExecutionResponse{}, gatewayError("unsupported_protocol", core.CategoryUnsupportedFeature, "Unsupported response protocol")
	}
	requestCtx, cancel := context.WithCancel(ctx)
	resp, err := t.Do(requestCtx, in)
	if err != nil {
		cancel()
		return core.ExecutionResponse{}, transportError(err)
	}
	unsupportedEncoding := false
	for _, value := range resp.Header.Values("Content-Encoding") {
		for _, token := range strings.Split(value, ",") {
			if !strings.EqualFold(strings.TrimSpace(token), "identity") {
				unsupportedEncoding = true
			}
		}
	}
	if unsupportedEncoding {
		resp.Body.Close()
		cancel()
		return core.ExecutionResponse{}, gatewayError("unsupported_response_encoding", core.CategoryUnavailable, fmt.Sprintf("Unsupported upstream response encoding (HTTP %d)", resp.StatusCode))
	}
	headers := resp.Header.Clone()
	blocked := map[string]bool{"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true, "set-cookie": true, "authorization": true, "proxy-connection": true}
	for _, value := range headers.Values("Connection") {
		for _, field := range strings.Split(value, ",") {
			blocked[strings.ToLower(strings.TrimSpace(field))] = true
		}
	}
	for name := range headers {
		if blocked[strings.ToLower(name)] {
			delete(headers, name)
		}
	}
	status := resp.StatusCode
	head := &core.HeadFrame{Protocol: protocol, ContentType: resp.Header.Get("Content-Type"), HTTPStatus: &status, Headers: headers}
	if status < 200 || status >= 300 {
		head.Error = rejectionError(status, resp.Header.Get("Retry-After"))
	}
	return core.ExecutionResponse{Stream: &fixedStream{body: resp.Body, cancel: cancel, requestCtx: requestCtx, head: head}}, nil
}

type fixedStream struct {
	mu         sync.Mutex
	body       io.ReadCloser
	cancel     context.CancelFunc
	requestCtx context.Context
	head       *core.HeadFrame
	phase      int // head, body, complete, closed
	seen       []byte
	over       bool
	pending    error
}

func (s *fixedStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == 3 {
		return nil
	}
	s.phase = 3
	s.cancel()
	return s.body.Close()
}

func (s *fixedStream) Next(ctx context.Context) (core.StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		s.Close()
		return core.StreamFrame{}, err
	}
	s.mu.Lock()
	switch s.phase {
	case 0:
		s.phase = 1
		head := s.head
		s.mu.Unlock()
		return core.StreamFrame{Type: core.FrameHead, Head: head}, nil
	case 2:
		s.mu.Unlock()
		return core.StreamFrame{}, io.EOF
	case 3:
		s.mu.Unlock()
		return core.StreamFrame{}, context.Canceled
	}
	s.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	buf := make([]byte, chunkSize)
	var n int
	var err error
	if s.pending != nil {
		err = s.pending
	} else {
		for n == 0 && err == nil && ctx.Err() == nil {
			n, err = s.body.Read(buf)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == 3 || ctx.Err() != nil {
		if ctx.Err() != nil {
			return core.StreamFrame{}, ctx.Err()
		}
		return core.StreamFrame{}, context.Canceled
	}
	if n > 0 {
		s.pending = err
		if !s.over && len(s.seen)+n <= observationLimit {
			s.seen = append(s.seen, buf[:n]...)
		} else {
			s.over = true
			s.seen = nil
		}
		// Defer EOF/error classification until after the delivered bytes.
		return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: buf[:n]}}, nil
	}
	s.phase = 2
	s.body.Close()
	complete := s.completion(err)
	s.cancel()
	return core.StreamFrame{Type: core.FrameComplete, Complete: complete}, nil
}

func (s *fixedStream) completion(readErr error) *core.CompleteFrame {
	unknown := &core.UsageReport{Source: core.UsageUnknown, Completeness: core.UsageUnknownCompleteness}
	if s.requestCtx.Err() != nil {
		return &core.CompleteFrame{Outcome: core.OutcomeCancelled, Error: transportError(s.requestCtx.Err()), Usage: unknown}
	}
	if s.head.Error != nil {
		return &core.CompleteFrame{Outcome: core.OutcomeFailed, Error: s.head.Error, Usage: unknown}
	}
	if readErr != io.EOF || s.over {
		return &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: gatewayError("incomplete_response", core.CategoryUnavailable, "Upstream response was incomplete"), Usage: unknown}
	}
	fields, ok := distinctFields(s.seen, "status", "error", "usage")
	if !ok {
		return &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: gatewayError("invalid_response", core.CategoryUnavailable, "Upstream response has no valid terminal status"), Usage: unknown}
	}
	var status string
	if err := json.Unmarshal(fields["status"], &status); err != nil || status == "" {
		return &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: gatewayError("invalid_response", core.CategoryUnavailable, "Upstream response has no valid terminal status"), Usage: unknown}
	}
	usage := unknown
	if rawUsage := fields["usage"]; len(rawUsage) > 0 && string(rawUsage) != "null" {
		var counts struct {
			Input        *int64 `json:"input_tokens"`
			Output       *int64 `json:"output_tokens"`
			InputDetails struct {
				Cached *int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails struct {
				Reasoning *int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		}
		usageFields, valid := distinctFields(rawUsage, "input_tokens", "output_tokens", "input_tokens_details", "output_tokens_details")
		if detail, ok := usageFields["input_tokens_details"]; ok && string(detail) != "null" {
			_, valid = distinctFields(detail, "cached_tokens")
		}
		if detail, ok := usageFields["output_tokens_details"]; ok && string(detail) != "null" && valid {
			_, valid = distinctFields(detail, "reasoning_tokens")
		}
		if valid && json.Unmarshal(rawUsage, &counts) == nil && counts.Input != nil && counts.Output != nil && *counts.Input >= 0 && *counts.Output >= 0 &&
			(counts.InputDetails.Cached == nil || (*counts.InputDetails.Cached >= 0 && *counts.InputDetails.Cached <= *counts.Input)) &&
			(counts.OutputDetails.Reasoning == nil || (*counts.OutputDetails.Reasoning >= 0 && *counts.OutputDetails.Reasoning <= *counts.Output)) {
			usage = &core.UsageReport{InputTokens: counts.Input, OutputTokens: counts.Output, Source: core.UsageProvider, Completeness: core.UsageComplete}
			usage.CachedTokens = counts.InputDetails.Cached
			usage.ReasoningTokens = counts.OutputDetails.Reasoning
		}
	}
	if rawError := fields["error"]; status == "failed" || (len(rawError) > 0 && string(rawError) != "null") {
		return &core.CompleteFrame{Outcome: core.OutcomeFailed, Error: gatewayError("provider_failure", core.CategoryUnavailable, "Upstream reported a failure"), Usage: usage}
	}
	if status != "completed" {
		return &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: gatewayError("incomplete_response", core.CategoryUnavailable, "Upstream response was incomplete"), Usage: usage}
	}
	return &core.CompleteFrame{Outcome: core.OutcomeSucceeded, Usage: usage}
}

// distinctFields rejects duplicates and case-folded aliases of observed
// metadata, including escaped spellings, without rejecting opaque fields.
func distinctFields(data []byte, observed ...string) (map[string]json.RawMessage, bool) {
	if !json.Valid(data) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		name, ok := key.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		for _, field := range observed {
			if strings.EqualFold(name, field) {
				if name != field {
					return nil, false
				}
				if fields[field] != nil {
					return nil, false
				}
				fields[field] = value
				break
			}
		}
	}
	_, err = decoder.Token()
	return fields, err == nil
}

func gatewayError(code string, category core.ErrorCategory, message string) *core.GatewayError {
	return &core.GatewayError{Code: code, Category: category, Message: message, RetryDisposition: core.RetryUnknown}
}

func transportError(err error) *core.GatewayError {
	if errors.Is(err, context.Canceled) {
		return gatewayError("upstream_cancelled", core.CategoryCancelled, "Upstream request cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return gatewayError("upstream_timeout", core.CategoryTimeout, "Upstream request timed out")
	}
	return gatewayError("upstream_unavailable", core.CategoryUnavailable, "Upstream request failed")
}

func rejectionError(status int, retryAfter string) *core.GatewayError {
	category := core.CategoryUnavailable
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		category = core.CategoryInvalidRequest
	case http.StatusUnauthorized:
		category = core.CategoryUnauthenticated
	case http.StatusForbidden:
		category = core.CategoryPermissionDenied
	case http.StatusTooManyRequests:
		category = core.CategoryRateLimited
	}
	err := gatewayError("upstream_rejection", category, fmt.Sprintf("Upstream rejected request (HTTP %d)", status))
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		err.Retryable = true
		err.RetryAfter = parseRetryAfter(retryAfter, time.Now())
	}
	return err
}

func parseRetryAfter(value string, now time.Time) *time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 && seconds <= int64((time.Duration(1<<63-1))/time.Second) {
		delay := time.Duration(seconds) * time.Second
		return &delay
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		delay := at.Sub(now)
		return &delay
	}
	return nil
}

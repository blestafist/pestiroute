package antigravity

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	maxSSELine   = 1 << 20
	maxEventSize = 1 << 20
)

var (
	errMalformedSSEJSON        = errors.New("malformed SSE JSON")
	errMalformedCandidateFrame = errors.New("malformed candidate frame")
	errSSEBounds               = errors.New("SSE bounds exceeded")
	errUnsupportedFinish       = errors.New("unsupported finish reason")
	errUnsupportedReasoning    = errors.New("unsupported reasoning signature")
	errInvalidUsage            = errors.New("invalid usage")
	errInvalidFunction         = errors.New("invalid function call")
	errEmptyContent            = errors.New("empty candidate content")
	errInvalidContent          = errors.New("invalid candidate content")
	errInvalidOrder            = errors.New("invalid candidate frame order")
)

type invalidContentSubkind string

const (
	invalidContentDoneWithCandidates   invalidContentSubkind = "done_with_candidates"
	invalidContentTextWithFunctionCall invalidContentSubkind = "text_with_function_call"
	invalidContentInvalidUTF8Text      invalidContentSubkind = "invalid_utf8_text"
)

type invalidContentError struct{ subkind invalidContentSubkind }

func (e invalidContentError) Error() string        { return errInvalidContent.Error() }
func (e invalidContentError) Is(target error) bool { return target == errInvalidContent }

type usageMetadata struct {
	Prompt     *int64 `json:"promptTokenCount"`
	Candidates *int64 `json:"candidatesTokenCount"`
	Total      *int64 `json:"totalTokenCount"`
}

type streamFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	ID   string          `json:"id"`
}
type streamPart struct {
	text                                          string
	thought                                       bool
	call                                          *streamFunctionCall
	args, callID, itemID                          string
	signature                                     []byte
	providerPartID                                *string
	candidateIndex, partIndex, signaturePartIndex int
	association                                   string
}

type stream struct {
	r                  *bufio.Reader
	body               io.ReadCloser
	mu                 sync.Mutex
	logger             *slog.Logger
	closed             bool
	once               sync.Once
	closeErr           error
	phase              uint8
	pending            [][]byte
	terminal           bool
	sequence           int
	responseID         string
	messageID          string
	currentMessageID   string
	text               strings.Builder
	outputs            []map[string]any
	outputIndex        int
	usedCallIDs        map[string]bool
	seenSignatures     map[string]bool
	retainedBytes      int
	messageStarted     bool
	messageIndex       int
	reasoningStarted   bool
	reasoningIndex     int
	reasoningID        string
	reasoningText      strings.Builder
	reasoningEncrypted string
	carrierBytes       int
	codec              *ReasoningCodec
	includeReasoning   bool
	scope              reasoningScope
	usage              *core.UsageReport
	reasoningKind      string
	signatureType      string
	sawFinish          bool
	sawTrailingUsage   bool
	skipLF             bool
}

func newStream(body io.ReadCloser, responseID, messageID string, logger ...*slog.Logger) *stream {
	return newReasoningStream(body, responseID, messageID, loggerOrNil(logger), nil, false, reasoningScope{})
}
func loggerOrNil(logger []*slog.Logger) *slog.Logger {
	if len(logger) != 0 {
		return logger[0]
	}
	return nil
}
func newReasoningStream(body io.ReadCloser, responseID, messageID string, logger *slog.Logger, codec *ReasoningCodec, include bool, scope reasoningScope) *stream {
	s := &stream{r: bufio.NewReader(body), body: body, responseID: responseID, messageID: messageID, messageIndex: -1}
	s.logger, s.codec, s.includeReasoning, s.scope = logger, codec, include && codec != nil, scope
	return s
}
func (s *stream) Close() error {
	s.once.Do(func() { s.mu.Lock(); s.closed = true; s.mu.Unlock(); s.closeErr = s.body.Close() })
	return s.closeErr
}
func (s *stream) isClosed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
func (s *stream) Next(ctx context.Context) (core.StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return core.StreamFrame{}, err
	}
	if s.phase == 3 {
		return core.StreamFrame{}, io.EOF
	}
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	if s.isClosed() {
		return core.StreamFrame{}, context.Canceled
	}
	if s.phase == 2 {
		if len(s.pending) > 0 {
			return bodyFrame(s.pop()), nil
		}
		s.phase = 3
		_ = s.Close()
		return core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded, Usage: s.usage}}, nil
	}
	if s.phase == 0 {
		e, err := s.nextEvent(ctx)
		if err != nil {
			s.logStreamError(err, s.phase == 0)
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			_ = s.Close()
			return core.StreamFrame{}, err
		}
		if err = s.consume(e); err != nil {
			s.logStreamError(err, s.phase == 0)
			_ = s.Close()
			return core.StreamFrame{}, err
		}
		s.phase = 1
		status := 200
		return core.StreamFrame{Type: core.FrameHead, Head: &core.HeadFrame{Protocol: protocol, HTTPStatus: &status, ContentType: "text/event-stream"}}, nil
	}
	if len(s.pending) > 0 {
		return bodyFrame(s.pop()), nil
	}
	for !s.terminal {
		e, err := s.nextEvent(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) && s.sawFinish {
				s.terminal = true
				break
			}
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			if ctx.Err() != nil {
				_ = s.Close()
				return core.StreamFrame{}, ctx.Err()
			}
			s.logStreamError(err, false)
			_ = s.Close()
			return core.StreamFrame{}, err
		}
		if err = s.consume(e); err != nil {
			s.logStreamError(err, false)
			_ = s.Close()
			return core.StreamFrame{}, err
		}
		if len(s.pending) > 0 {
			return bodyFrame(s.pop()), nil
		}
	}
	if len(s.outputs) == 0 {
		err := errEmptyContent
		s.logStreamError(err, false)
		_ = s.Close()
		return core.StreamFrame{}, err
	}
	s.phase = 2
	if s.outputs == nil {
		s.outputs = make([]map[string]any, 0)
	}
	final := map[string]any{"id": s.responseID, "object": "response", "status": "completed", "output": s.outputs}
	s.closeReasoning()
	s.closeMessage()
	final["output"] = s.outputs
	if s.usage != nil {
		u := map[string]int64{}
		if s.usage.InputTokens != nil {
			u["input_tokens"] = *s.usage.InputTokens
		}
		if s.usage.OutputTokens != nil {
			u["output_tokens"] = *s.usage.OutputTokens
		}
		if len(u) > 0 {
			final["usage"] = u
		}
	}
	s.pending = append(s.pending, emitEvent("response.completed", map[string]any{"response": final}, &s.sequence))
	s.phase = 2
	if len(s.pending) > 0 {
		return bodyFrame(s.pop()), nil
	}
	s.phase = 3
	_ = s.Close()
	return core.StreamFrame{Type: core.FrameComplete, Complete: &core.CompleteFrame{Outcome: core.OutcomeSucceeded, Usage: s.usage}}, nil
}

func (s *stream) logStreamError(err error, precommit bool) {
	if s.logger == nil {
		return
	}
	phase, category := "postcommit", "postcommit_error"
	if precommit {
		phase, category = "precommit", "precommit_framing"
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			category = "precommit_eof"
		} else if errors.Is(err, errSSEBounds) {
			category = "precommit_bounds_exceeded"
		} else if errors.Is(err, errMalformedSSEJSON) || errors.Is(err, errMalformedCandidateFrame) {
			category = "precommit_invalid_json"
		} else if errors.Is(err, errUnsupportedFinish) {
			category = "precommit_unsupported_finish"
		} else if errors.Is(err, errUnsupportedReasoning) {
			category = "precommit_unsupported_reasoning"
		} else if errors.Is(err, errInvalidUsage) {
			category = "precommit_invalid_usage"
		} else if errors.Is(err, errInvalidFunction) {
			category = "precommit_invalid_function"
		} else if errors.Is(err, errEmptyContent) {
			category = "precommit_empty_content"
		} else if errors.Is(err, errInvalidContent) {
			category = "precommit_invalid_content"
		} else if errors.Is(err, errInvalidOrder) {
			category = "precommit_invalid_order"
		}
	} else {
		cause := postcommitCause(err)
		if errors.Is(err, errUnsupportedReasoning) {
			s.logger.Debug("Antigravity stream failure", "phase", phase, "category", category, "cause", cause, "kind", s.reasoningKind, "signature_type", s.signatureType)
			return
		}
		var invalid invalidContentError
		if errors.As(err, &invalid) {
			switch invalid.subkind {
			case invalidContentDoneWithCandidates, invalidContentTextWithFunctionCall, invalidContentInvalidUTF8Text:
				s.logger.Debug("Antigravity stream failure", "phase", phase, "category", category, "cause", cause, "subkind", invalid.subkind)
				return
			}
		}
		s.logger.Debug("Antigravity stream failure", "phase", phase, "category", category, "cause", cause)
		return
	}
	if category == "precommit_unsupported_reasoning" {
		s.logger.Debug("Antigravity stream failure", "phase", phase, "category", category, "kind", s.reasoningKind, "signature_type", s.signatureType)
		return
	}
	var invalid invalidContentError
	if category == "precommit_invalid_content" && errors.As(err, &invalid) {
		switch invalid.subkind {
		case invalidContentDoneWithCandidates, invalidContentTextWithFunctionCall, invalidContentInvalidUTF8Text:
			s.logger.Debug("Antigravity stream failure", "phase", phase, "category", category, "subkind", invalid.subkind)
			return
		}
	}
	s.logger.Debug("Antigravity stream failure", "phase", phase, "category", category)
}

func postcommitCause(err error) string {
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	case errors.Is(err, errSSEBounds):
		return "bounds_exceeded"
	case errors.Is(err, errMalformedSSEJSON), errors.Is(err, errMalformedCandidateFrame):
		return "invalid_json"
	case errors.Is(err, errUnsupportedFinish):
		return "unsupported_finish"
	case errors.Is(err, errUnsupportedReasoning):
		return "unsupported_reasoning"
	case errors.Is(err, errInvalidUsage):
		return "invalid_usage"
	case errors.Is(err, errInvalidFunction):
		return "invalid_function"
	case errors.Is(err, errEmptyContent):
		return "empty_content"
	case errors.Is(err, errInvalidContent):
		return "invalid_content"
	case errors.Is(err, errInvalidOrder):
		return "invalid_order"
	default:
		return "framing"
	}
}

func classifySignature(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "absent"
	}
	switch raw[0] {
	case '"':
		return "string"
	case 'n':
		return "null"
	default:
		return "other"
	}
}
func (s *stream) pop() []byte { b := s.pending[0]; s.pending = s.pending[1:]; return b }
func bodyFrame(b []byte) core.StreamFrame {
	return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: b}}
}
func (s *stream) nextEvent(ctx context.Context) ([]byte, error) {
	var data []byte
	hasData := false
	firstLine := true
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := s.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && hasData {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if firstLine {
			line = strings.TrimPrefix(line, "\xef\xbb\xbf")
			firstLine = false
		}
		if line == "" {
			if hasData {
				return data, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		if field != "data" {
			continue
		}
		if hasData {
			data = append(data, '\n')
		}
		if len(data)+len(value) > maxEventSize {
			return nil, errSSEBounds
		}
		data = append(data, value...)
		hasData = true
	}
}

func (s *stream) readLine() (string, error) {
	var line []byte
	for {
		b, err := s.r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 {
				return "", io.EOF
			}
			if errors.Is(err, io.EOF) {
				return string(line), nil
			}
			return "", err
		}
		if s.skipLF {
			s.skipLF = false
			if b == '\n' {
				continue
			}
		}
		if b == '\n' {
			return string(line), nil
		}
		if b == '\r' {
			s.skipLF = true
			return string(line), nil
		}
		if len(line) >= maxSSELine {
			return "", errSSEBounds
		}
		line = append(line, b)
	}
}

func (s *stream) consume(raw []byte) error {
	if !utf8.Valid(raw) || !uniqueJSON(raw) {
		return errMalformedSSEJSON
	}
	type functionCall = streamFunctionCall
	var outer struct {
		Response struct {
			Candidates []struct {
				FinishReason string `json:"finishReason"`
				Content      struct {
					Parts []struct {
						Text             *string         `json:"text"`
						FunctionCall     json.RawMessage `json:"functionCall"`
						Thought          *bool           `json:"thought"`
						ThoughtSignature json.RawMessage `json:"thoughtSignature"`
						ID               *string         `json:"id"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			Done  *bool           `json:"done"`
			Usage json.RawMessage `json:"usageMetadata"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &outer) != nil {
		return errMalformedCandidateFrame
	}
	resp := outer.Response
	usage, err := parseUsageMetadata(resp.Usage)
	if err != nil {
		return err
	}
	if s.terminal {
		return errInvalidOrder
	}
	if s.sawFinish {
		if s.sawTrailingUsage || resp.Done != nil || len(resp.Candidates) != 0 || usage == nil || !usageOnlyFrame(raw) {
			return errInvalidOrder
		}
		if err := s.setUsage(usage); err != nil {
			return err
		}
		s.sawTrailingUsage = true
		return nil
	}
	if resp.Done != nil && *resp.Done {
		if len(resp.Candidates) > 0 {
			return invalidContentError{subkind: invalidContentDoneWithCandidates}
		}
		if usage != nil {
			if usage.Prompt != nil || usage.Candidates != nil {
				completeness := core.UsagePartial
				if usage.Prompt != nil && usage.Candidates != nil {
					completeness = core.UsageComplete
				}
				s.usage = &core.UsageReport{InputTokens: usage.Prompt, OutputTokens: usage.Candidates, Source: core.UsageProvider, Completeness: completeness}
			}
		}
		s.terminal = true
		return nil
	}
	var parts []streamPart
	finished := false
	sawEmptyText := false
	retainedBytes := s.retainedBytes
	var stagedCallIDs map[string]bool
	stagedSignatures := make(map[string]bool)
	var stagedUsage *core.UsageReport
	for candidateIndex, candidate := range resp.Candidates {
		if candidate.FinishReason != "" {
			if candidate.FinishReason != "STOP" {
				return errUnsupportedFinish
			}
			finished = true
		}
		for partIndex, part := range candidate.Content.Parts {
			signature, signaturePresent := partSignature(part.ThoughtSignature)
			if len(part.ThoughtSignature) != 0 && !signaturePresent {
				thought := part.Thought != nil && *part.Thought
				s.reasoningKind = "signature_only"
				if thought {
					s.reasoningKind = "both"
				}
				s.signatureType = classifySignature(part.ThoughtSignature)
				return errUnsupportedReasoning
			}
			if signaturePresent && part.Text == nil && len(part.FunctionCall) == 0 {
				s.reasoningKind, s.signatureType = "signature_only", classifySignature(part.ThoughtSignature)
				if part.Thought != nil && *part.Thought {
					s.reasoningKind = "both"
				}
				if part.Thought != nil && *part.Thought || len(parts) == 0 || parts[len(parts)-1].signature != nil || parts[len(parts)-1].candidateIndex != candidateIndex || parts[len(parts)-1].partIndex != partIndex-1 {
					return errUnsupportedReasoning
				}
				previous := &parts[len(parts)-1]
				if !s.includeReasoning {
					s.reasoningKind, s.signatureType = "signature_only", "string"
					return errUnsupportedReasoning
				}
				key := string(signature)
				if s.seenSignatures[key] || stagedSignatures[key] {
					return errUnsupportedReasoning
				}
				stagedSignatures[key] = true
				previous.signature, previous.signaturePartIndex, previous.association = signature, partIndex, "adjacent"
				continue
			}
			if part.Thought != nil && *part.Thought && part.Text == nil {
				thought := part.Thought != nil && *part.Thought
				signature := signaturePresent
				s.reasoningKind = "thought_only"
				if thought && signature {
					s.reasoningKind = "both"
				} else if thought {
					s.reasoningKind = "thought_only"
				}
				s.signatureType = classifySignature(part.ThoughtSignature)
				return errUnsupportedReasoning
			}
			if signaturePresent && !s.includeReasoning {
				s.reasoningKind, s.signatureType = "signature_only", classifySignature(part.ThoughtSignature)
				if part.Thought != nil && *part.Thought {
					s.reasoningKind = "both"
				}
				return errUnsupportedReasoning
			}
			if signaturePresent {
				key := string(signature)
				if s.seenSignatures[key] || stagedSignatures[key] {
					return errUnsupportedReasoning
				}
				stagedSignatures[key] = true
			}
			if part.Text != nil && len(part.FunctionCall) != 0 {
				return invalidContentError{subkind: invalidContentTextWithFunctionCall}
			}
			if part.Text != nil {
				if !utf8.ValidString(*part.Text) {
					return invalidContentError{subkind: invalidContentInvalidUTF8Text}
				}
				if *part.Text == "" {
					sawEmptyText = true
					continue
				}
				if len(*part.Text) > maxRequestBytes-retainedBytes {
					return errSSEBounds
				}
				retainedBytes += len(*part.Text)
				parsed := streamPart{text: *part.Text, thought: part.Thought != nil && *part.Thought, providerPartID: cloneString(part.ID), candidateIndex: candidateIndex, partIndex: partIndex, signaturePartIndex: partIndex, association: "direct"}
				if signaturePresent {
					parsed.signature = signature
				}
				parts = append(parts, parsed)
				continue
			}
			if len(part.FunctionCall) != 0 {
				var fields map[string]json.RawMessage
				if json.Unmarshal(part.FunctionCall, &fields) != nil || fields == nil || fields["name"] == nil || fields["args"] == nil {
					return errInvalidFunction
				}
				for key := range fields {
					if key != "name" && key != "args" && key != "id" {
						return errInvalidFunction
					}
				}
				call := new(functionCall)
				if json.Unmarshal(part.FunctionCall, call) != nil {
					return errInvalidFunction
				}
				if len(call.Args) > maxRequestBytes-retainedBytes {
					return errSSEBounds
				}
				if !validFunctionName(call.Name) || len(call.Args) == 0 || !uniqueJSON(call.Args) || !utf8.Valid(call.Args) {
					return errInvalidFunction
				}
				trimmed := strings.TrimSpace(string(call.Args))
				if len(trimmed) == 0 || trimmed[0] != '{' {
					return errInvalidFunction
				}
				var object map[string]json.RawMessage
				if json.Unmarshal(call.Args, &object) != nil || object == nil {
					return errInvalidFunction
				}
				arguments := string(call.Args)
				if idRaw, present := fields["id"]; present && (json.Unmarshal(idRaw, &call.ID) != nil || call.ID == "") {
					return errInvalidFunction
				}
				if call.ID != "" {
					if len(call.ID) > 256 || !utf8.ValidString(call.ID) || strings.TrimSpace(call.ID) != call.ID {
						return errInvalidFunction
					}
				}
				callIndex := len(s.usedCallIDs) + len(stagedCallIDs)
				callID := call.ID
				if callID == "" {
					callID = "call_" + s.responseID + "_" + strconv.Itoa(callIndex)
				}
				if s.usedCallIDs[callID] || stagedCallIDs[callID] {
					return errInvalidFunction
				}
				if stagedCallIDs == nil {
					stagedCallIDs = make(map[string]bool)
				}
				stagedCallIDs[callID] = true
				itemID := "fc_" + s.responseID + "_" + strconv.Itoa(callIndex)
				retainedBytes += len(arguments)
				parsed := streamPart{call: call, args: arguments, callID: callID, itemID: itemID, providerPartID: cloneString(part.ID), candidateIndex: candidateIndex, partIndex: partIndex, signaturePartIndex: partIndex, association: "direct"}
				if signaturePresent {
					parsed.signature = signature
				}
				parts = append(parts, parsed)
				continue
			}
			return errEmptyContent
		}
	}
	if resp.Usage != nil {
		var err error
		stagedUsage, err = parseUsage(usage)
		if err != nil {
			return err
		}
	}
	if len(resp.Candidates) == 0 && usage == nil {
		return errEmptyContent
	}
	// Commit staged frame state only after every part and usage value validates.
	if stagedUsage != nil {
		s.usage = stagedUsage
	}
	if finished {
		s.sawFinish = true
	}
	if len(stagedCallIDs) > 0 {
		if s.usedCallIDs == nil {
			s.usedCallIDs = make(map[string]bool, len(stagedCallIDs))
		}
		for id := range stagedCallIDs {
			s.usedCallIDs[id] = true
		}
	}
	if len(stagedSignatures) != 0 {
		if s.seenSignatures == nil {
			s.seenSignatures = make(map[string]bool, len(stagedSignatures))
		}
		for signature := range stagedSignatures {
			s.seenSignatures[signature] = true
		}
	}
	if len(resp.Candidates) == 0 {
		return nil
	}
	for _, part := range parts {
		if s.sequence == 0 {
			s.pending = append(s.pending, emitEvent("response.created", map[string]any{"response": map[string]any{"id": s.responseID, "object": "response", "status": "in_progress", "output": []any{}}}, &s.sequence))
		}
		if part.thought {
			if part.signature != nil {
				s.closeMessage()
			}
			if part.signature != nil && s.reasoningStarted {
				s.closeReasoning()
			}
			if !s.reasoningStarted {
				idx := s.outputIndex
				s.outputIndex++
				s.reasoningIndex = idx
				s.reasoningID = "rs_" + s.responseID + "_" + strconv.Itoa(idx)
				s.reasoningText.Reset()
				s.reasoningStarted = true
				item := map[string]any{"id": s.reasoningID, "type": "reasoning", "summary": []any{}, "status": "in_progress"}
				s.outputs = append(s.outputs, item)
				s.pending = append(s.pending,
					emitEvent("response.output_item.added", map[string]any{"output_index": idx, "item": item}, &s.sequence),
					emitEvent("response.reasoning_summary_part.added", map[string]any{"item_id": s.reasoningID, "output_index": idx, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}, &s.sequence),
				)
			}
			s.reasoningText.WriteString(part.text)
			s.pending = append(s.pending, emitEvent("response.reasoning_summary_text.delta", map[string]any{"item_id": s.reasoningID, "output_index": s.reasoningIndex, "summary_index": 0, "delta": part.text}, &s.sequence))
			if part.signature != nil {
				carrier, err := s.sealPart(part, s.reasoningID, s.reasoningIndex, nil, nil)
				if err != nil {
					return err
				}
				s.reasoningEncrypted = carrier
				s.closeReasoning()
			}
			continue
		}
		s.closeReasoning()
		if part.call != nil && part.signature != nil {
			s.closeMessage()
		}
		if part.call == nil {
			if part.signature != nil {
				s.closeMessage()
			}
			if !s.messageStarted {
				itemID := s.messageID
				if s.outputIndex != 0 {
					itemID += "_" + strconv.Itoa(s.outputIndex)
				}
				s.currentMessageID = itemID
				item := map[string]any{"id": itemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}}
				s.outputs = append(s.outputs, item)
				s.messageIndex = s.outputIndex
				s.outputIndex++
				s.messageStarted = true
				s.pending = append(s.pending, emitEvent("response.output_item.added", map[string]any{"output_index": s.messageIndex, "item": item}, &s.sequence))
			}
			s.text.WriteString(part.text)
			content := s.outputs[s.messageIndex]["content"].([]any)[0].(map[string]any)
			content["text"] = s.text.String()
			s.pending = append(s.pending, emitEvent("response.output_text.delta", map[string]any{"item_id": s.currentMessageID, "output_index": s.messageIndex, "content_index": 0, "delta": part.text}, &s.sequence))
			if part.signature != nil {
				id := s.currentMessageID
				s.closeMessage()
				carrier, err := s.sealPart(part, "rs_"+s.responseID+"_"+strconv.Itoa(s.outputIndex), s.outputIndex, &id, nil)
				if err != nil {
					return err
				}
				s.addCarrierItem(carrier)
			}
			continue
		}
		if part.signature != nil {
			callID := part.callID
			carrierIndex := s.outputIndex
			carrier, err := s.sealPart(part, "rs_"+s.responseID+"_"+strconv.Itoa(carrierIndex), carrierIndex, nil, &callID)
			if err != nil {
				return err
			}
			s.addCarrierItem(carrier)
		}
		index := s.outputIndex
		s.outputIndex++
		item := map[string]any{"id": part.itemID, "type": "function_call", "call_id": part.callID, "name": part.call.Name, "arguments": part.args, "status": "completed"}
		s.outputs = append(s.outputs, item)
		s.pending = append(s.pending,
			emitEvent("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"id": part.itemID, "type": "function_call", "call_id": part.callID, "name": part.call.Name, "arguments": "", "status": "in_progress"}}, &s.sequence),
			emitEvent("response.function_call_arguments.delta", map[string]any{"item_id": part.itemID, "output_index": index, "delta": part.args}, &s.sequence),
			emitEvent("response.function_call_arguments.done", map[string]any{"item_id": part.itemID, "output_index": index, "arguments": part.args}, &s.sequence),
			emitEvent("response.output_item.done", map[string]any{"output_index": index, "item": item}, &s.sequence),
		)
	}
	s.retainedBytes = retainedBytes
	if len(s.pending) == 0 && !finished && usage == nil && !sawEmptyText {
		return errEmptyContent
	}
	return nil
}

func usageOnlyFrame(raw []byte) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || len(envelope) != 1 {
		return false
	}
	responseRaw, ok := envelope["response"]
	if !ok {
		return false
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(responseRaw, &response) != nil || len(response) != 1 {
		return false
	}
	usageRaw, ok := response["usageMetadata"]
	if !ok {
		return false
	}
	var usage map[string]json.RawMessage
	if json.Unmarshal(usageRaw, &usage) != nil || usage == nil {
		return false
	}
	for key := range usage {
		if key != "promptTokenCount" && key != "candidatesTokenCount" && key != "totalTokenCount" {
			return false
		}
	}
	return true
}

func (s *stream) setUsage(usage *usageMetadata) error {
	report, err := parseUsage(usage)
	if err != nil {
		return err
	}
	if report != nil {
		s.usage = report
	}
	return nil
}

func parseUsageMetadata(raw json.RawMessage) (*usageMetadata, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errInvalidUsage
	}
	for key := range fields {
		if key != "promptTokenCount" && key != "candidatesTokenCount" && key != "totalTokenCount" {
			return nil, errInvalidUsage
		}
	}
	var usage usageMetadata
	for key, value := range fields {
		count, err := strconv.ParseInt(strings.TrimSpace(string(value)), 10, 64)
		if err != nil || count < 0 {
			return nil, errInvalidUsage
		}
		countPtr := new(int64)
		*countPtr = count
		switch key {
		case "promptTokenCount":
			usage.Prompt = countPtr
		case "candidatesTokenCount":
			usage.Candidates = countPtr
		case "totalTokenCount":
			usage.Total = countPtr
		}
	}
	return &usage, nil
}

func parseUsage(usage *usageMetadata) (*core.UsageReport, error) {
	if usage != nil {
		if (usage.Prompt != nil && *usage.Prompt < 0) || (usage.Candidates != nil && *usage.Candidates < 0) || (usage.Total != nil && *usage.Total < 0) {
			return nil, errInvalidUsage
		}
	}
	if usage != nil && (usage.Prompt != nil || usage.Candidates != nil) {
		completeness := core.UsagePartial
		if usage.Prompt != nil && usage.Candidates != nil {
			completeness = core.UsageComplete
		}
		return &core.UsageReport{InputTokens: usage.Prompt, OutputTokens: usage.Candidates, Source: core.UsageProvider, Completeness: completeness}, nil
	}
	return nil, nil
}

func (s *stream) closeReasoning() {
	if !s.reasoningStarted {
		return
	}
	text := s.reasoningText.String()
	item := s.outputs[s.reasoningIndex]
	item["summary"] = []any{map[string]any{"type": "summary_text", "text": text}}
	item["status"] = "completed"
	if s.reasoningEncrypted != "" {
		item["encrypted_content"] = s.reasoningEncrypted
		s.reasoningEncrypted = ""
	}
	s.pending = append(s.pending,
		emitEvent("response.reasoning_summary_text.done", map[string]any{"item_id": s.reasoningID, "output_index": s.reasoningIndex, "summary_index": 0, "text": text}, &s.sequence),
		emitEvent("response.reasoning_summary_part.done", map[string]any{"item_id": s.reasoningID, "output_index": s.reasoningIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text}}, &s.sequence),
		emitEvent("response.output_item.done", map[string]any{"output_index": s.reasoningIndex, "item": item}, &s.sequence),
	)
	s.reasoningStarted = false
	s.reasoningID = ""
	s.reasoningText.Reset()
}

func (s *stream) closeMessage() {
	if !s.messageStarted {
		return
	}
	item := s.outputs[s.messageIndex]
	item["status"] = "completed"
	content := item["content"].([]any)[0].(map[string]any)
	s.pending = append(s.pending,
		emitEvent("response.output_text.done", map[string]any{"item_id": s.currentMessageID, "output_index": s.messageIndex, "content_index": 0, "text": content["text"]}, &s.sequence),
		emitEvent("response.output_item.done", map[string]any{"output_index": s.messageIndex, "item": item}, &s.sequence),
	)
	s.messageStarted, s.currentMessageID = false, ""
	s.text.Reset()
}

func partSignature(raw json.RawMessage) ([]byte, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return nil, false
	}
	return []byte(value), true
}

func (s *stream) sealPart(part streamPart, reasoningID string, outputIndex int, associatedItemID, associatedCallID *string) (string, error) {
	kind := "text"
	var content *string
	var call *reasoningFunctionCall
	if part.call != nil {
		kind = "function_call"
		args := append(json.RawMessage(nil), part.call.Args...)
		var id *string
		if part.call.ID != "" {
			id = &part.call.ID
		}
		call = &reasoningFunctionCall{Name: part.call.Name, Args: args, ID: id}
	} else {
		value := part.text
		content = &value
		if part.thought {
			kind = "thought"
		}
	}
	issued := s.codec.clock()
	value := reasoningPlaintext{
		SchemaVersion: 1,
		Source:        reasoningSource{CandidateIndex: part.candidateIndex, PartIndex: part.partIndex, PartKind: kind, ProviderPartID: cloneString(part.providerPartID), SignatureBytesB64: base64.StdEncoding.EncodeToString(part.signature), Content: content, FunctionCall: call},
		Association:   reasoningAssociation{Kind: part.association, SignaturePartIndex: part.signaturePartIndex, SourcePartIndex: part.partIndex},
		Responses:     reasoningResponses{ReasoningItemID: reasoningID, OutputIndex: outputIndex, AssociatedItemID: associatedItemID, AssociatedCallID: associatedCallID},
	}
	raw, err := s.codec.seal(value, s.scope, issued, issued.Add(24*time.Hour))
	if err != nil || len(raw) > reasoningCarrierLimit-s.carrierBytes {
		return "", errUnsupportedReasoning
	}
	s.carrierBytes += len(raw)
	return string(raw), nil
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func (s *stream) addCarrierItem(carrier string) {
	idx := s.outputIndex
	s.outputIndex++
	id := "rs_" + s.responseID + "_" + strconv.Itoa(idx)
	item := map[string]any{"id": id, "type": "reasoning", "summary": []any{}, "status": "completed", "encrypted_content": carrier}
	s.outputs = append(s.outputs, item)
	s.pending = append(s.pending,
		emitEvent("response.output_item.added", map[string]any{"output_index": idx, "item": map[string]any{"id": id, "type": "reasoning", "summary": []any{}, "status": "in_progress"}}, &s.sequence),
		emitEvent("response.output_item.done", map[string]any{"output_index": idx, "item": item}, &s.sequence))
}
func emitEvent(name string, payload map[string]any, seq *int) []byte {
	payload["type"] = name
	payload["sequence_number"] = *seq
	*seq++
	b, _ := json.Marshal(payload)
	return []byte("event: " + name + "\ndata: " + string(b) + "\n\n")
}

var _ core.Stream = (*stream)(nil)

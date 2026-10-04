package anthropic

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

const maxRetainedText = 1 << 20

var errResponsesLifecycle = errors.New("invalid Responses emitter lifecycle")

type responsesEmitter struct {
	sequence    int64
	responseID  string
	itemID      string
	text        []byte
	arguments   []byte
	callID      string
	name        string
	itemType    string
	output      []responseItem
	callIDs     map[string]struct{}
	retained    int
	outputIndex int
	started     bool
	itemAdded   bool
	itemDone    bool
	closed      bool
	budgetHit   bool
}

func newResponsesEmitter() (*responsesEmitter, error) {
	responseID, err := newResponseID("resp_")
	if err != nil {
		return nil, err
	}
	itemID, err := newResponseID("msg_")
	if err != nil {
		return nil, err
	}
	return &responsesEmitter{responseID: responseID, itemID: itemID}, nil
}

func newResponseID(prefix string) (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate Responses ID: %w", err)
	}
	return prefix + hex.EncodeToString(id[:]), nil
}

func (e *responsesEmitter) Start() ([][]byte, error) {
	frames, err := e.StartResponse()
	if err != nil {
		return nil, err
	}
	text, err := e.StartText()
	if err != nil {
		return nil, err
	}
	return append(frames, text...), nil
}

func (e *responsesEmitter) StartResponse() ([][]byte, error) {
	if e.started || e.closed {
		return nil, errResponsesLifecycle
	}
	e.started = true
	return [][]byte{
		e.event("response.created", responseLifecycleEvent{Response: responseEnvelope{ID: e.responseID, Object: "response", Status: "in_progress", Output: []responseItem{}}}),
		e.event("response.in_progress", responseLifecycleEvent{Response: responseEnvelope{ID: e.responseID, Object: "response", Status: "in_progress", Output: []responseItem{}}}),
	}, nil
}

func (e *responsesEmitter) StartText() ([][]byte, error) {
	if !e.started || e.closed || (e.itemAdded && !e.itemDone) {
		return nil, errResponsesLifecycle
	}
	if e.itemAdded {
		id, err := newResponseID("msg_")
		if err != nil {
			return nil, err
		}
		e.itemID = id
	}
	e.itemAdded, e.itemDone, e.itemType = true, false, "message"
	e.outputIndex, e.text = len(e.output), nil
	return [][]byte{
		e.event("response.output_item.added", responseItemEvent{OutputIndex: e.outputIndex, Item: responseItem{ID: e.itemID, Type: "message", Role: "assistant", Status: "in_progress", Content: []responsePart{}}}),
		e.event("response.content_part.added", responsePartEvent{OutputIndex: e.outputIndex, ItemID: e.itemID, Part: responsePart{Type: "output_text", Text: "", Annotations: []any{}, Logprobs: []any{}}}),
	}, nil
}

func (e *responsesEmitter) StartTool(callID, name string) ([]byte, error) {
	if !e.started || e.closed || (e.itemAdded && !e.itemDone) || callID == "" || !validToolName(name) {
		return nil, errResponsesLifecycle
	}
	if e.callIDs == nil {
		e.callIDs = make(map[string]struct{})
	}
	if _, exists := e.callIDs[callID]; exists {
		return nil, errors.New("duplicate Anthropic tool call ID")
	}
	itemID, err := newResponseID("fc_")
	if err != nil {
		return nil, err
	}
	e.itemID, e.callID, e.name, e.itemType, e.itemAdded, e.itemDone = itemID, callID, name, "function_call", true, false
	e.outputIndex = len(e.output)
	e.arguments = nil
	e.callIDs[callID] = struct{}{}
	empty := ""
	return e.event("response.output_item.added", responseItemEvent{OutputIndex: e.outputIndex, Item: responseItem{ID: itemID, Type: "function_call", CallID: callID, Name: name, Arguments: &empty, Status: "in_progress"}}), nil
}

func (e *responsesEmitter) ToolDelta(delta string) ([]byte, error) {
	if !e.started || e.closed || !e.itemAdded || e.itemType != "function_call" || e.itemDone {
		return nil, errResponsesLifecycle
	}
	if !utf8.ValidString(delta) {
		e.closed, e.arguments = true, nil
		return nil, errors.New("tool arguments delta is not valid UTF-8")
	}
	if len(delta) > maxRetainedText-e.retained {
		e.closed, e.budgetHit, e.arguments = true, true, nil
		return nil, errors.New("tool arguments exceed 1 MiB")
	}
	e.arguments = append(e.arguments, delta...)
	e.retained += len(delta)
	return e.event("response.function_call_arguments.delta", responseDelta{OutputIndex: e.outputIndex, ItemID: e.itemID, Delta: delta}), nil
}

func (e *responsesEmitter) FinishTool() ([][]byte, error) {
	return e.finishTool(false)
}

func (e *responsesEmitter) finishTool(truncated bool) ([][]byte, error) {
	if !e.started || e.closed || !e.itemAdded || e.itemType != "function_call" || e.itemDone {
		return nil, errResponsesLifecycle
	}
	arguments := string(e.arguments)
	// A tool with no argument deltas represents an empty input object.
	if arguments == "" {
		if maxRetainedText-e.retained < 2 {
			e.closed, e.budgetHit = true, true
			return nil, errors.New("tool arguments exceed 1 MiB")
		}
		arguments = "{}"
		e.retained += len(arguments)
	}
	status := "completed"
	if truncated {
		status = "incomplete"
	} else if !validSchemaObject(json.RawMessage(arguments)) {
		return nil, errors.New("invalid Anthropic tool arguments")
	}
	e.itemDone = true
	item := responseItem{ID: e.itemID, Type: "function_call", CallID: e.callID, Name: e.name, Arguments: &arguments, Status: status}
	e.output = append(e.output, item)
	e.arguments = nil
	return [][]byte{
		e.event("response.function_call_arguments.done", responseFunctionArgumentsDone{ItemID: e.itemID, OutputIndex: e.outputIndex, Arguments: arguments}),
		e.event("response.output_item.done", responseItemEvent{OutputIndex: e.outputIndex, Item: item}),
	}, nil
}

func (e *responsesEmitter) Delta(text string) ([]byte, error) {
	if !e.started || e.closed || !e.itemAdded || e.itemDone || e.itemType != "message" {
		return nil, errResponsesLifecycle
	}
	if !utf8.ValidString(text) {
		e.closed = true
		e.text = nil
		return nil, errors.New("Responses delta is not valid UTF-8")
	}
	if len(text) > maxRetainedText-e.retained {
		e.closed, e.budgetHit = true, true
		e.text = nil
		return nil, errors.New("Responses retained text exceeds 1 MiB")
	}
	e.text = append(e.text, text...)
	e.retained += len(text)
	return e.event("response.output_text.delta", responseTextDelta{responseDelta: responseDelta{OutputIndex: e.outputIndex, ItemID: e.itemID, Delta: text}, Logprobs: []any{}}), nil
}

func (e *responsesEmitter) FinishText() ([][]byte, error) {
	if !e.started || e.closed || !e.itemAdded || e.itemDone || e.itemType != "message" {
		return nil, errResponsesLifecycle
	}
	e.itemDone = true
	text := string(e.text)
	part := responsePart{Type: "output_text", Text: text, Annotations: []any{}, Logprobs: []any{}}
	item := responseItem{ID: e.itemID, Type: "message", Role: "assistant", Status: "completed", Content: []responsePart{part}}
	e.output = append(e.output, item)
	e.text = nil
	return [][]byte{
		e.event("response.output_text.done", responseDone{OutputIndex: e.outputIndex, ItemID: e.itemID, Text: text, Logprobs: []any{}}),
		e.event("response.content_part.done", responsePartEvent{OutputIndex: e.outputIndex, ItemID: e.itemID, Part: part}),
		e.event("response.output_item.done", responseItemEvent{OutputIndex: e.outputIndex, Item: item}),
	}, nil
}

func (e *responsesEmitter) Finish(stopReason string, inputTokens, outputTokens, cachedTokens *int64) ([][]byte, error) {
	if !e.started || e.closed || !e.itemAdded || invalidCount(inputTokens) || invalidCount(outputTokens) || invalidCount(cachedTokens) {
		return nil, errResponsesLifecycle
	}
	totalTokens, err := addCounts(inputTokens, outputTokens)
	if err != nil {
		return nil, err
	}
	var terminal, status, reason string
	switch stopReason {
	case "end_turn", "stop_sequence", "tool_use":
		terminal, status = "response.completed", "completed"
	case "max_tokens":
		terminal, status, reason = "response.incomplete", "incomplete", "max_output_tokens"
	default:
		return nil, fmt.Errorf("unsupported Anthropic stop reason %q", stopReason)
	}
	if (stopReason == "tool_use") != (len(e.callIDs) > 0) && stopReason != "max_tokens" {
		return nil, errors.New("stop reason does not match output items")
	}
	var frames [][]byte
	if !e.itemDone {
		if e.itemType == "function_call" {
			frames, err = e.finishTool(stopReason == "max_tokens")
		} else {
			frames, err = e.FinishText()
		}
		if err != nil {
			return nil, err
		}
	}
	e.closed = true
	response := responseEnvelope{ID: e.responseID, Object: "response", Status: status, Output: append([]responseItem(nil), e.output...), Usage: responseUsageFor(inputTokens, outputTokens, totalTokens, cachedTokens)}
	if reason != "" {
		response.IncompleteDetails = &responseIncompleteDetails{Reason: reason}
	}
	frames = append(frames, e.event(terminal, responseLifecycleEvent{Response: response}))
	return frames, nil
}

func (e *responsesEmitter) Failed(code, message string, input, output, cached *int64) ([]byte, error) {
	if !e.started || (e.closed && !e.budgetHit) {
		return nil, errResponsesLifecycle
	}
	e.closed, e.budgetHit = true, false
	response := responseEnvelope{ID: e.responseID, Object: "response", Status: "failed", Output: append([]responseItem(nil), e.output...), Usage: partialResponseUsage(input, output, cached)}
	wireCode := "server_error"
	if code == "upstream_rate_limited" {
		wireCode = "rate_limit_exceeded"
	}
	response.Error = &responseError{Code: wireCode, Message: message}
	return e.event("response.failed", responseLifecycleEvent{Response: response}), nil
}

func (e *responsesEmitter) Incomplete(input, output, cached *int64) ([]byte, error) {
	if !e.started || e.closed {
		return nil, errResponsesLifecycle
	}
	e.closed = true
	response := responseEnvelope{ID: e.responseID, Object: "response", Status: "incomplete", Output: append([]responseItem(nil), e.output...), Usage: partialResponseUsage(input, output, cached), IncompleteDetails: &responseIncompleteDetails{Reason: "incomplete_response"}}
	return e.event("response.incomplete", responseLifecycleEvent{Response: response}), nil
}

func partialResponseUsage(input, output, cached *int64) *responseUsage {
	total, err := addCounts(input, output)
	if err != nil {
		total = nil
	}
	return responseUsageFor(input, output, total, cached)
}

type responseLifecycleEvent struct {
	Response responseEnvelope `json:"response"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responseEnvelope struct {
	Error             *responseError             `json:"error,omitempty"`
	ID                string                     `json:"id"`
	Object            string                     `json:"object"`
	Status            string                     `json:"status"`
	Output            []responseItem             `json:"output"`
	Usage             *responseUsage             `json:"usage,omitempty"`
	IncompleteDetails *responseIncompleteDetails `json:"incomplete_details,omitempty"`
}

type responseUsage struct {
	InputTokens        *int64              `json:"input_tokens,omitempty"`
	OutputTokens       *int64              `json:"output_tokens,omitempty"`
	TotalTokens        *int64              `json:"total_tokens,omitempty"`
	InputTokensDetails *inputTokensDetails `json:"input_tokens_details,omitempty"`
}

type inputTokensDetails struct {
	CachedTokens *int64 `json:"cached_tokens,omitempty"`
}

func responseUsageFor(input, output, total, cached *int64) *responseUsage {
	usage := &responseUsage{InputTokens: input, OutputTokens: output, TotalTokens: total}
	if cached != nil {
		usage.InputTokensDetails = &inputTokensDetails{CachedTokens: cached}
	}
	return usage
}

type responseIncompleteDetails struct {
	Reason string `json:"reason"`
}

type responseItem struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Role      string         `json:"role,omitempty"`
	Status    string         `json:"status,omitempty"`
	Content   []responsePart `json:"content,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Arguments *string        `json:"arguments,omitempty"`
}

type responseFunctionArgumentsDone struct {
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	Arguments   string `json:"arguments"`
}

type responsePart struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
	Logprobs    []any  `json:"logprobs"`
}

type responseDelta struct {
	OutputIndex int    `json:"output_index"`
	ItemID      string `json:"item_id"`
	Delta       string `json:"delta"`
}

type responseTextDelta struct {
	responseDelta
	ContentIndex int   `json:"content_index"`
	Logprobs     []any `json:"logprobs"`
}

type responseDone struct {
	ContentIndex int    `json:"content_index"`
	Logprobs     []any  `json:"logprobs"`
	OutputIndex  int    `json:"output_index"`
	ItemID       string `json:"item_id"`
	Text         string `json:"text"`
}

type responseItemEvent struct {
	OutputIndex int          `json:"output_index"`
	Item        responseItem `json:"item"`
}

type responsePartEvent struct {
	ContentIndex int          `json:"content_index"`
	OutputIndex  int          `json:"output_index"`
	ItemID       string       `json:"item_id"`
	Part         responsePart `json:"part"`
}

func (e *responsesEmitter) event(name string, payload any) []byte {
	data, _ := json.Marshal(payload)
	typeField, _ := json.Marshal(name)
	prefix := fmt.Sprintf(`{"type":%s,"sequence_number":%d,`, typeField, e.sequence)
	e.sequence++
	data = append([]byte(prefix), data[1:]...)
	frame := make([]byte, 0, len(name)+len(data)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, name...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	return frame
}

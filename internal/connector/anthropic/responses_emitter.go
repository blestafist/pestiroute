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
		responseEvent("response.created", responseLifecycleEvent{Response: responseEnvelope{ID: e.responseID, Object: "response", Status: "in_progress", Output: []responseItem{}}}),
		responseEvent("response.in_progress", responseLifecycleEvent{Response: responseEnvelope{ID: e.responseID, Object: "response", Status: "in_progress", Output: []responseItem{}}}),
	}, nil
}

func (e *responsesEmitter) StartText() ([][]byte, error) {
	if !e.started || e.closed || e.itemAdded {
		return nil, errResponsesLifecycle
	}
	e.itemAdded, e.itemType = true, "message"
	return [][]byte{
		responseEvent("response.output_item.added", responseItemEvent{OutputIndex: 0, Item: responseItem{ID: e.itemID, Type: "message", Role: "assistant", Status: "in_progress", Content: []responsePart{}}}),
		responseEvent("response.content_part.added", responsePartEvent{OutputIndex: 0, ItemID: e.itemID, Part: responsePart{Type: "output_text", Text: "", Annotations: []any{}, Logprobs: []any{}}}),
	}, nil
}

func (e *responsesEmitter) StartTool(callID, name string) ([]byte, error) {
	if !e.started || e.closed || (e.itemAdded && !e.itemDone) || callID == "" || name == "" {
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
	return responseEvent("response.output_item.added", responseItemEvent{OutputIndex: e.outputIndex, Item: responseItem{ID: itemID, Type: "function_call", CallID: callID, Name: name, Arguments: &empty, Status: "in_progress"}}), nil
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
		e.closed, e.arguments = true, nil
		return nil, errors.New("tool arguments exceed 1 MiB")
	}
	e.arguments = append(e.arguments, delta...)
	e.retained += len(delta)
	return responseEvent("response.function_call_arguments.delta", responseDelta{OutputIndex: e.outputIndex, ItemID: e.itemID, Delta: delta}), nil
}

func (e *responsesEmitter) FinishTool() ([][]byte, error) {
	if !e.started || e.closed || !e.itemAdded || e.itemType != "function_call" || e.itemDone {
		return nil, errResponsesLifecycle
	}
	e.itemDone = true
	arguments := string(e.arguments)
	item := responseItem{ID: e.itemID, Type: "function_call", CallID: e.callID, Name: e.name, Arguments: &arguments, Status: "completed"}
	e.output = append(e.output, item)
	return [][]byte{
		responseEvent("response.function_call_arguments.done", responseFunctionArgumentsDone{ItemID: e.itemID, OutputIndex: e.outputIndex, Arguments: arguments}),
		responseEvent("response.output_item.done", responseItemEvent{OutputIndex: e.outputIndex, Item: item}),
	}, nil
}

func (e *responsesEmitter) Delta(text string) ([]byte, error) {
	if !e.started || e.closed || !e.itemAdded || e.itemType != "message" {
		return nil, errResponsesLifecycle
	}
	if !utf8.ValidString(text) {
		e.closed = true
		e.text = nil
		return nil, errors.New("Responses delta is not valid UTF-8")
	}
	if len(text) > maxRetainedText-e.retained {
		e.closed = true
		e.text = nil
		return nil, errors.New("Responses retained text exceeds 1 MiB")
	}
	e.text = append(e.text, text...)
	e.retained += len(text)
	return responseEvent("response.output_text.delta", responseDelta{OutputIndex: 0, ItemID: e.itemID, Delta: text}), nil
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
	if e.itemType == "function_call" {
		if stopReason != "tool_use" || !e.itemDone || len(e.output) == 0 {
			e.closed = true
			return nil, errors.New("incomplete tool call stream")
		}
		response := responseEnvelope{ID: e.responseID, Object: "response", Status: status, Output: append([]responseItem(nil), e.output...), Usage: responseUsageFor(inputTokens, outputTokens, totalTokens, cachedTokens)}
		e.closed = true
		return [][]byte{responseEvent(terminal, responseLifecycleEvent{Response: response})}, nil
	}
	if stopReason == "tool_use" {
		return nil, errors.New("tool_use stop without tool call")
	}
	e.closed = true
	text := string(e.text)
	part := responsePart{Type: "output_text", Text: text, Annotations: []any{}, Logprobs: []any{}}
	item := responseItem{ID: e.itemID, Type: "message", Role: "assistant", Status: "completed", Content: []responsePart{part}}
	frames := [][]byte{
		responseEvent("response.output_text.done", responseDone{OutputIndex: 0, ItemID: e.itemID, Text: text}),
		responseEvent("response.content_part.done", responsePartEvent{OutputIndex: 0, ItemID: e.itemID, Part: part}),
		responseEvent("response.output_item.done", responseItemEvent{OutputIndex: 0, Item: item}),
	}
	response := responseEnvelope{ID: e.responseID, Object: "response", Status: status, Output: []responseItem{item}, Usage: responseUsageFor(inputTokens, outputTokens, totalTokens, cachedTokens)}
	if reason != "" {
		response.IncompleteDetails = &responseIncompleteDetails{Reason: reason}
	}
	frames = append(frames, responseEvent(terminal, responseLifecycleEvent{Response: response}))
	return frames, nil
}

func (e *responsesEmitter) Failed(code, message string, input, output, cached *int64) ([]byte, error) {
	if !e.started || e.closed {
		return nil, errResponsesLifecycle
	}
	e.closed = true
	response := responseEnvelope{ID: e.responseID, Object: "response", Status: "failed", Output: append([]responseItem(nil), e.output...), Usage: partialResponseUsage(input, output, cached)}
	return responseEvent("response.failed", struct {
		Response responseEnvelope `json:"response"`
		Error    struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Response: response, Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}}), nil
}

func (e *responsesEmitter) Incomplete(input, output, cached *int64) ([]byte, error) {
	if !e.started || e.closed {
		return nil, errResponsesLifecycle
	}
	e.closed = true
	response := responseEnvelope{ID: e.responseID, Object: "response", Status: "incomplete", Output: append([]responseItem(nil), e.output...), Usage: partialResponseUsage(input, output, cached), IncompleteDetails: &responseIncompleteDetails{Reason: "incomplete_response"}}
	return responseEvent("response.incomplete", responseLifecycleEvent{Response: response}), nil
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

type responseEnvelope struct {
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

type responseDone struct {
	OutputIndex int    `json:"output_index"`
	ItemID      string `json:"item_id"`
	Text        string `json:"text"`
}

type responseItemEvent struct {
	OutputIndex int          `json:"output_index"`
	Item        responseItem `json:"item"`
}

type responsePartEvent struct {
	OutputIndex int          `json:"output_index"`
	ItemID      string       `json:"item_id"`
	Part        responsePart `json:"part"`
}

func responseEvent(name string, payload any) []byte {
	data, _ := json.Marshal(payload)
	typeField, _ := json.Marshal(name)
	data = append(append([]byte(`{"type":`), typeField...), append([]byte(","), data[1:]...)...)
	frame := make([]byte, 0, len(name)+len(data)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, name...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	return frame
}

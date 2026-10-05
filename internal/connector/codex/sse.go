package codex

import (
	"encoding/json"
	"errors"
	"strconv"
	"unicode/utf8"

	"github.com/blestafist/pestiroute/internal/core"
)

const (
	maxSSEEventBytes  = 1 << 20
	maxSSEScalarBytes = 4 << 10
)

var errInvalidSSE = errors.New("invalid or oversized SSE framing")

// sseObserver validates framing without retaining or rewriting the response.
// Its only variable-size buffer is one bounded line of the current event.
type sseObserver struct {
	line          []byte
	eventBytes    int
	dataLines     int
	eventName     string
	data          []byte
	lastEvent     string
	events        uint64
	terminal      *core.CompleteFrame
	usage         *core.UsageReport
	terminalUsage bool
	cr            bool
	err           error
}

func (o *sseObserver) feed(p []byte) {
	for _, b := range p {
		if o.err != nil {
			return
		}
		if o.cr {
			o.cr = false
			if b == '\n' {
				continue
			}
		}
		if o.eventBytes == maxSSEEventBytes {
			o.err = errInvalidSSE
			return
		}
		o.eventBytes++
		if b == '\r' || b == '\n' {
			o.endLine()
			if b == '\r' {
				o.cr = true
			}
			continue
		}
		if len(o.line) >= maxSSEEventBytes {
			o.err = errInvalidSSE
			return
		}
		o.line = append(o.line, b)
	}
}

func (o *sseObserver) endLine() {
	line := o.line
	o.line = nil
	if len(line) == 0 {
		o.endEvent()
		return
	}
	if line[0] == ':' { // Comment / keepalive.
		if len(line) > maxSSEScalarBytes || !utf8.Valid(line) {
			o.err = errInvalidSSE
		}
		return
	}
	field, value, hasValue := line, []byte(nil), false
	for i, b := range line {
		if b == ':' {
			field, value, hasValue = line[:i], line[i+1:], true
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			break
		}
	}
	if !utf8.Valid(field) || !utf8.Valid(value) {
		o.err = errInvalidSSE
		return
	}
	switch string(field) {
	case "data":
		o.dataLines++
		if len(o.data)+len(value)+1 > maxSSEEventBytes {
			o.err = errInvalidSSE
			return
		}
		o.data = append(o.data, value...)
		o.data = append(o.data, '\n')
	case "event":
		if len(value) > maxSSEScalarBytes {
			o.err = errInvalidSSE
			return
		}
		o.eventName = string(value)
	case "id", "retry":
		if len(value) > maxSSEScalarBytes {
			o.err = errInvalidSSE
			return
		}
	default:
		// Unknown SSE fields remain opaque and are only framing-validated.
		if len(field) > maxSSEScalarBytes || hasValue && len(value) > maxSSEScalarBytes {
			o.err = errInvalidSSE
		}
	}
}

func (o *sseObserver) endEvent() {
	if o.dataLines > 0 {
		o.events++
		o.lastEvent = o.eventName
		usageFound := false
		if usage, found, invalid := observeUsage(o.data); found {
			usageFound = true
			if invalid {
				o.usage = unknownCodexUsage()
			} else {
				o.usage = usage
			}
		}
		if o.terminal != nil {
			o.terminal = incompleteTerminal("Upstream sent data after its terminal event")
		} else if terminal := observeTerminal(o.eventName, o.data); terminal != nil {
			o.terminal = terminal
			o.terminalUsage = usageFound
		}
	}
	o.dataLines = 0
	o.eventName = ""
	o.data = nil
	o.eventBytes = 0
}

// observeUsage extracts accounting metadata without changing the SSE bytes.
// Missing or null counters remain unknown; inclusive totals are never summed
// with their cached/reasoning subsets.
func observeUsage(data []byte) (*core.UsageReport, bool, bool) {
	var payload struct {
		Usage    json.RawMessage `json:"usage"`
		Response struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, false, false
	}
	raw := payload.Response.Usage
	if len(raw) == 0 {
		raw = payload.Usage
	}
	if len(raw) == 0 {
		return nil, false, false
	}
	unknown := unknownCodexUsage()
	if string(raw) == "null" {
		return unknown, true, false
	}
	var fields struct {
		Input      json.RawMessage `json:"input_tokens"`
		Output     json.RawMessage `json:"output_tokens"`
		InputInfo  json.RawMessage `json:"input_tokens_details"`
		OutputInfo json.RawMessage `json:"output_tokens_details"`
	}
	if json.Unmarshal(raw, &fields) != nil || len(raw) == 0 || raw[0] != '{' {
		return unknown, true, true
	}
	var input, output, cached, reasoning *int64
	var invalid bool
	input, invalid = parseUsageCounter(fields.Input)
	if invalid {
		return unknown, true, true
	}
	output, invalid = parseUsageCounter(fields.Output)
	if invalid {
		return unknown, true, true
	}
	if len(fields.InputInfo) > 0 && string(fields.InputInfo) != "null" {
		var details struct {
			Cached json.RawMessage `json:"cached_tokens"`
		}
		if json.Unmarshal(fields.InputInfo, &details) != nil || fields.InputInfo[0] != '{' {
			return unknown, true, true
		}
		cached, invalid = parseUsageCounter(details.Cached)
		if invalid {
			return unknown, true, true
		}
	}
	if len(fields.OutputInfo) > 0 && string(fields.OutputInfo) != "null" {
		var details struct {
			Reasoning json.RawMessage `json:"reasoning_tokens"`
		}
		if json.Unmarshal(fields.OutputInfo, &details) != nil || fields.OutputInfo[0] != '{' {
			return unknown, true, true
		}
		reasoning, invalid = parseUsageCounter(details.Reasoning)
		if invalid {
			return unknown, true, true
		}
	}
	if input != nil && cached != nil && *cached > *input || output != nil && reasoning != nil && *reasoning > *output {
		return unknown, true, true
	}
	source := core.UsageProvider
	if input == nil && output == nil && cached == nil && reasoning == nil {
		source = core.UsageUnknown
	}
	report := &core.UsageReport{InputTokens: input, OutputTokens: output, CachedTokens: cached, ReasoningTokens: reasoning, Source: source, Completeness: core.UsageUnknownCompleteness}
	if input != nil && output != nil {
		report.Completeness = core.UsagePartial
	}
	return report, true, false
}

func parseUsageCounter(raw json.RawMessage) (*int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || value < 0 {
		return nil, true
	}
	return &value, false
}

func unknownCodexUsage() *core.UsageReport {
	return &core.UsageReport{Source: core.UsageUnknown, Completeness: core.UsageUnknownCompleteness}
}

func (o *sseObserver) finalUsage(outcome core.Outcome) *core.UsageReport {
	if o.usage == nil {
		return unknownCodexUsage()
	}
	usage := *o.usage
	if usage.InputTokens != nil && usage.OutputTokens != nil && usage.Source == core.UsageProvider {
		usage.Completeness = core.UsagePartial
		if outcome == core.OutcomeSucceeded && o.terminalUsage {
			usage.Completeness = core.UsageComplete
		}
	}
	return &usage
}

func incompleteTerminal(message string) *core.CompleteFrame {
	return &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: connectorError("invalid_response", core.CategoryUnavailable, message)}
}

func observeTerminal(event string, data []byte) *core.CompleteFrame {
	var payload struct {
		Type     string `json:"type"`
		Response struct {
			Status string `json:"status"`
		} `json:"response"`
	}
	validJSON := json.Unmarshal(data, &payload) == nil
	kind := event
	if kind == "" && validJSON {
		kind = payload.Type
	}
	switch kind {
	case "response.completed", "response.failed", "response.incomplete", "error":
	default:
		return nil
	}
	if !validJSON || payload.Type != kind || event != "" && payload.Type != event {
		return incompleteTerminal("Upstream terminal event was invalid")
	}
	switch kind {
	case "response.completed":
		if payload.Response.Status != "completed" {
			return incompleteTerminal("Upstream terminal event was invalid")
		}
		return &core.CompleteFrame{Outcome: core.OutcomeSucceeded}
	case "response.failed", "error":
		return &core.CompleteFrame{Outcome: core.OutcomeFailed, Error: connectorError("provider_failure", core.CategoryUnavailable, "Upstream reported a failure")}
	default:
		return &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: connectorError("incomplete_response", core.CategoryUnavailable, "Upstream response was incomplete")}
	}
}

func (o *sseObserver) finish() error {
	if o.err != nil {
		return o.err
	}
	if len(o.line) != 0 || o.eventBytes != 0 {
		return errInvalidSSE
	}
	return nil
}

package chatcompletions

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const jsonFrameSize = 32 << 10

type jsonResult struct {
	usage *core.UsageReport
	err   error
}

// jsonStream pipes bounded chunks into a token decoder while returning the
// original bytes to the caller. Response size never determines retained memory.
type jsonStream struct {
	mu          sync.Mutex
	body        io.ReadCloser
	head        *core.HeadFrame
	writer      *io.PipeWriter
	parsed      <-chan jsonResult
	pending     error
	phase       int
	cancel      context.CancelFunc
	idleTimeout time.Duration
	idleExpired atomic.Bool
}

func newJSONStream(body io.ReadCloser, head *core.HeadFrame, cancel context.CancelFunc, idleTimeout time.Duration) *jsonStream {
	r, w := io.Pipe()
	parsed := make(chan jsonResult, 1)
	go func() {
		usage, err := observeJSON(r)
		if err != nil {
			_, _ = io.Copy(io.Discard, r)
		}
		_ = r.Close()
		parsed <- jsonResult{usage: usage, err: err}
	}()
	return &jsonStream{body: body, head: head, writer: w, parsed: parsed, cancel: cancel, idleTimeout: idleTimeout}
}

func (s *jsonStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == 3 {
		return nil
	}
	s.phase = 3
	_ = s.writer.CloseWithError(context.Canceled)
	err := s.body.Close()
	s.cancel()
	return err
}

func (s *jsonStream) Next(ctx context.Context) (core.StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return core.StreamFrame{}, err
	}
	s.mu.Lock()
	if s.phase == 0 {
		s.phase = 1
		h := s.head
		s.mu.Unlock()
		return core.StreamFrame{Type: core.FrameHead, Head: h}, nil
	}
	if s.phase >= 2 {
		s.mu.Unlock()
		return core.StreamFrame{}, io.EOF
	}
	s.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	buf := make([]byte, jsonFrameSize)
	var n int
	var readErr error
	if s.pending != nil {
		readErr, s.pending = s.pending, nil
	} else {
		var timer *time.Timer
		var timerDone chan struct{}
		if s.idleTimeout > 0 {
			timerDone = make(chan struct{})
			timer = time.AfterFunc(s.idleTimeout, func() {
				defer close(timerDone)
				s.idleExpired.Store(true)
				_ = s.body.Close()
				s.cancel()
			})
		}
		n, readErr = s.body.Read(buf)
		if timer != nil && !timer.Stop() {
			<-timerDone
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == 3 {
		return core.StreamFrame{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return core.StreamFrame{}, err
	}
	if s.idleExpired.Load() {
		s.phase = 2
		_ = s.writer.CloseWithError(io.ErrClosedPipe)
		_ = s.body.Close()
		result := <-s.parsed
		complete := &core.CompleteFrame{Outcome: core.OutcomeIncomplete, Error: ge("upstream_timeout", core.CategoryTimeout, "Upstream response body idle timeout"), Usage: result.usage}
		if complete.Usage == nil {
			complete.Usage = unknown()
		}
		s.cancel()
		return core.StreamFrame{Type: core.FrameComplete, Complete: complete}, nil
	}
	if n > 0 {
		s.pending = readErr
		if _, err := s.writer.Write(buf[:n]); err != nil {
			return core.StreamFrame{}, err
		}
		return core.StreamFrame{Type: core.FrameBody, Body: &core.BodyFrame{Data: buf[:n]}}, nil
	}
	s.phase = 2
	_ = s.body.Close()
	s.cancel()
	if readErr == io.EOF {
		_ = s.writer.Close()
	} else if readErr != nil {
		_ = s.writer.CloseWithError(readErr)
	} else {
		_ = s.writer.CloseWithError(io.ErrNoProgress)
	}
	result := <-s.parsed
	complete := &core.CompleteFrame{Outcome: core.OutcomeSucceeded, Usage: result.usage}
	if complete.Usage == nil {
		complete.Usage = unknown()
	}
	if s.head.Error != nil {
		complete.Outcome = core.OutcomeFailed
		complete.Error = s.head.Error
	} else if readErr != io.EOF {
		complete.Outcome = core.OutcomeIncomplete
		complete.Error = transportError(readErr)
	} else if result.err != nil {
		complete.Outcome = core.OutcomeIncomplete
		complete.Error = ge("invalid_response", core.CategoryUnavailable, "Upstream returned malformed JSON")
	}
	return core.StreamFrame{Type: core.FrameComplete, Complete: complete}, nil
}

type usageObserver struct {
	seen        map[string]bool
	values      map[string]json.Number
	usageObject bool
	invalid     bool
}

func observeJSON(input io.Reader) (*core.UsageReport, error) {
	o := usageObserver{seen: map[string]bool{}, values: map[string]json.Number{}}
	p := jsonScanner{r: bufio.NewReaderSize(input, 32<<10), usage: &o}
	if err := p.space(); err != nil {
		return nil, err
	}
	b, err := p.read()
	if err != nil {
		return nil, err
	}
	if b != '{' {
		return nil, errors.New("response JSON must be an object")
	}
	if err = p.object("", 0); err != nil {
		return nil, err
	}
	if err = p.space(); err != nil {
		return nil, err
	}
	if _, err = p.r.ReadByte(); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("trailing JSON value")
	}
	return o.report(), nil
}

const jsonTokenLimit = 128

type jsonScanner struct {
	r     *bufio.Reader
	usage *usageObserver
}

func (p *jsonScanner) read() (byte, error) { return p.r.ReadByte() }
func (p *jsonScanner) space() error {
	for {
		b, e := p.read()
		if e != nil {
			if e == io.EOF {
				return nil
			}
			return e
		}
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			return p.r.UnreadByte()
		}
	}
}
func (p *jsonScanner) object(parent string, depth int) error {
	if depth > 10000 {
		return errors.New("JSON nesting is too deep")
	}
	if err := p.space(); err != nil {
		return err
	}
	b, err := p.read()
	if err != nil {
		return err
	}
	if b == '}' {
		return nil
	}
	if err = p.r.UnreadByte(); err != nil {
		return err
	}
	for {
		key, err := p.string(true)
		if err != nil {
			return err
		}
		path, observed, alias := usagePath(parent, key)
		if alias {
			p.usage.invalid = true
		}
		if observed {
			if p.usage.seen[path] {
				p.usage.invalid = true
			}
			p.usage.seen[path] = true
		}
		if err = p.space(); err != nil {
			return err
		}
		b, err = p.read()
		if err != nil || b != ':' {
			return errors.New("invalid JSON object separator")
		}
		if err = p.value(path, depth+1); err != nil {
			return err
		}
		if err = p.space(); err != nil {
			return err
		}
		b, err = p.read()
		if err != nil {
			return err
		}
		if b == '}' {
			return nil
		}
		if b != ',' {
			return errors.New("invalid JSON object delimiter")
		}
		if err = p.space(); err != nil {
			return err
		}
	}
}
func (p *jsonScanner) array(path string, depth int) error {
	if depth > 10000 {
		return errors.New("JSON nesting is too deep")
	}
	if strings.HasPrefix(path, "usage.") {
		p.usage.invalid = true
	}
	if err := p.space(); err != nil {
		return err
	}
	b, err := p.read()
	if err != nil {
		return err
	}
	if b == ']' {
		return nil
	}
	if err = p.r.UnreadByte(); err != nil {
		return err
	}
	for {
		if err = p.value("", depth+1); err != nil {
			return err
		}
		if err = p.space(); err != nil {
			return err
		}
		b, err = p.read()
		if err != nil {
			return err
		}
		if b == ']' {
			return nil
		}
		if b != ',' {
			return errors.New("invalid JSON array delimiter")
		}
		if err = p.space(); err != nil {
			return err
		}
	}
}
func (p *jsonScanner) value(path string, depth int) error {
	if depth > 10000 {
		return errors.New("JSON nesting is too deep")
	}
	if err := p.space(); err != nil {
		return err
	}
	b, err := p.read()
	if err != nil {
		return err
	}
	switch b {
	case '{':
		if path == "usage" {
			p.usage.usageObject = true
		}
		if isUsageCount(path) {
			p.usage.invalid = true
		}
		return p.object(path, depth)
	case '[':
		return p.array(path, depth)
	case '"':
		if isUsageCount(path) || path == "usage.prompt_tokens_details" || path == "usage.completion_tokens_details" {
			p.usage.invalid = true
		}
		_, err = p.stringBody(false)
		return err
	case 't':
		err = p.literal("rue")
	case 'f':
		err = p.literal("alse")
	case 'n':
		err = p.literal("ull")
		if err == nil && path != "usage" && (isUsageCount(path) || path == "usage.prompt_tokens_details" || path == "usage.completion_tokens_details") {
			p.usage.invalid = true
		}
	default:
		if b == '-' || (b >= '0' && b <= '9') {
			return p.number(path, b)
		}
		return errors.New("invalid JSON value")
	}
	if err != nil {
		return err
	}
	if path == "usage" {
		p.usage.invalid = true
	}
	return nil
}
func (p *jsonScanner) string(capture bool) (string, error) {
	b, err := p.read()
	if err != nil || b != '"' {
		return "", errors.New("invalid JSON string")
	}
	raw, err := p.stringBody(capture)
	if err != nil {
		return "", err
	}
	if !capture || len(raw) == 0 {
		return "", nil
	}
	quoted := append(append([]byte{'"'}, raw...), '"')
	var key string
	if err = json.Unmarshal(quoted, &key); err != nil {
		return "", err
	}
	return key, nil
}
func (p *jsonScanner) stringBody(capture bool) ([]byte, error) {
	var out []byte
	long := false
	for {
		b, err := p.read()
		if err != nil {
			return nil, err
		}
		if b == '"' {
			if long {
				return nil, nil
			}
			return out, nil
		}
		if b < 0x20 {
			return nil, errors.New("invalid JSON string character")
		}
		if b == '\\' {
			esc, e := p.read()
			if e != nil {
				return nil, e
			}
			if capture {
				out = appendBounded(out, '\\', &long)
				out = appendBounded(out, esc, &long)
			}
			if esc == 'u' {
				for i := 0; i < 4; i++ {
					h, e := p.read()
					if e != nil || !isHex(h) {
						return nil, errors.New("invalid JSON unicode escape")
					}
					if capture {
						out = appendBounded(out, h, &long)
					}
				}
			} else if !strings.ContainsRune(`"\\/bfnrt`, rune(esc)) {
				return nil, errors.New("invalid JSON escape")
			}
			continue
		}
		if capture {
			out = appendBounded(out, b, &long)
		}
	}
}
func appendBounded(dst []byte, b byte, long *bool) []byte {
	if len(dst) >= jsonTokenLimit {
		*long = true
		return dst
	}
	return append(dst, b)
}
func isHex(b byte) bool { return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F' }
func (p *jsonScanner) literal(rest string) error {
	for i := 0; i < len(rest); i++ {
		b, e := p.read()
		if e != nil || b != rest[i] {
			return errors.New("invalid JSON literal")
		}
	}
	return nil
}
func (p *jsonScanner) number(path string, first byte) error {
	var token []byte
	keep := isUsageCount(path)
	add := func(b byte) {
		if keep && len(token) < jsonTokenLimit {
			token = append(token, b)
		} else if keep {
			p.usage.invalid = true
		}
	}
	b := first
	if b == '-' {
		add(b)
		var e error
		b, e = p.read()
		if e != nil || b < '0' || b > '9' {
			return errors.New("invalid JSON number")
		}
		add(b)
	} else {
		add(b)
	}
	if b == '0' {
		next, e := p.read()
		if e == nil {
			if next >= '0' && next <= '9' {
				return errors.New("invalid JSON number")
			}
			_ = p.r.UnreadByte()
		} else if e != io.EOF {
			return e
		}
	} else {
		for {
			next, e := p.read()
			if e != nil {
				if e == io.EOF {
					break
				}
				return e
			}
			if next < '0' || next > '9' {
				_ = p.r.UnreadByte()
				break
			}
			add(next)
		}
	}
	next, e := p.read()
	if e == nil && next == '.' {
		p.usage.invalid = p.usage.invalid || keep
		next, e = p.read()
		if e != nil || next < '0' || next > '9' {
			return errors.New("invalid JSON fraction")
		}
		for {
			next, e = p.read()
			if e != nil {
				if e == io.EOF {
					break
				}
				return e
			}
			if next < '0' || next > '9' {
				_ = p.r.UnreadByte()
				break
			}
		}
		next, e = p.read()
	}
	if e == nil && (next == 'e' || next == 'E') {
		p.usage.invalid = p.usage.invalid || keep
		next, e = p.read()
		if e != nil {
			return errors.New("invalid JSON exponent")
		}
		if next == '+' || next == '-' {
			next, e = p.read()
			if e != nil {
				return errors.New("invalid JSON exponent")
			}
		}
		if next < '0' || next > '9' {
			return errors.New("invalid JSON exponent")
		}
		for {
			next, e = p.read()
			if e != nil {
				if e == io.EOF {
					break
				}
				return e
			}
			if next < '0' || next > '9' {
				_ = p.r.UnreadByte()
				break
			}
		}
		next, e = p.read()
	}
	if e == nil {
		_ = p.r.UnreadByte()
	} else if e != io.EOF {
		return e
	}
	if keep {
		p.usage.values[path] = json.Number(token)
	}
	return nil
}

func usagePath(parent, key string) (string, bool, bool) {
	known := func(names ...string) (string, bool, bool) {
		for _, name := range names {
			if strings.EqualFold(key, name) {
				return parent + "." + name, true, key != name
			}
		}
		return "", false, false
	}
	switch parent {
	case "":
		if strings.EqualFold(key, "usage") {
			return "usage", true, key != "usage"
		}
	case "usage":
		return known("prompt_tokens", "completion_tokens", "total_tokens", "prompt_tokens_details", "completion_tokens_details")
	case "usage.prompt_tokens_details":
		return known("cached_tokens")
	case "usage.completion_tokens_details":
		return known("reasoning_tokens")
	}
	return "", false, false
}
func isUsageCount(path string) bool {
	return path == "usage.prompt_tokens" || path == "usage.completion_tokens" || path == "usage.prompt_tokens_details.cached_tokens" || path == "usage.completion_tokens_details.reasoning_tokens"
}

func (o *usageObserver) report() *core.UsageReport {
	if o.invalid || !o.usageObject || !o.seen["usage.prompt_tokens"] || !o.seen["usage.completion_tokens"] {
		return unknown()
	}
	in, errIn := usageCount(o.values["usage.prompt_tokens"])
	out, errOut := usageCount(o.values["usage.completion_tokens"])
	if errIn != nil || errOut != nil {
		return unknown()
	}
	r := &core.UsageReport{InputTokens: &in, OutputTokens: &out, Source: core.UsageProvider, Completeness: core.UsageComplete}
	for _, v := range []struct {
		path string
		max  int64
		dst  **int64
	}{{"usage.prompt_tokens_details.cached_tokens", in, &r.CachedTokens}, {"usage.completion_tokens_details.reasoning_tokens", out, &r.ReasoningTokens}} {
		if o.seen[v.path] {
			n, err := usageCount(o.values[v.path])
			if err != nil || n > v.max {
				return unknown()
			}
			*v.dst = &n
		}
	}
	return r
}
func usageCount(n json.Number) (int64, error) {
	v, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil || v < 0 {
		return 0, errors.New("invalid usage count")
	}
	return v, nil
}

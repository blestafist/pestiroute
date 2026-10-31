package chatcompletions

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

func Encode(w http.ResponseWriter, r *http.Request, resp core.ExecutionResponse, gatewayErr *core.GatewayError) error {
	if resp.Stream != nil {
		defer resp.Stream.Close()
	}
	if gatewayErr != nil {
		return writeError(w, gatewayErr)
	}
	if resp.Stream == nil || r == nil {
		return core.ErrStreamContract
	}
	committed := false
	for {
		if err := r.Context().Err(); err != nil {
			return err
		}
		f, err := resp.Stream.Next(r.Context())
		if err != nil {
			if !committed && r.Context().Err() == nil {
				return writeStreamFailure(w, err)
			}
			if errors.Is(err, io.EOF) {
				return core.ErrStreamContract
			}
			return err
		}
		switch f.Type {
		case core.FrameHead:
			if committed || f.Head == nil || f.Head.HTTPStatus == nil || *f.Head.HTTPStatus < 200 || *f.Head.HTTPStatus > 599 {
				return core.ErrStreamContract
			}
			copyHeaders(w.Header(), f.Head.Headers)
			if f.Head.ContentType != "" {
				w.Header().Set("Content-Type", f.Head.ContentType)
			}
			w.WriteHeader(*f.Head.HTTPStatus)
			committed = true
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		case core.FrameBody:
			if !committed || f.Body == nil {
				return core.ErrStreamContract
			}
			n, e := w.Write(f.Body.Data)
			if e != nil {
				return e
			}
			if n != len(f.Body.Data) {
				return io.ErrShortWrite
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		case core.FrameComplete:
			if !committed || f.Complete == nil {
				return core.ErrStreamContract
			}
			_, e := resp.Stream.Next(r.Context())
			if e == io.EOF {
				return nil
			}
			if e != nil {
				return e
			}
			return core.ErrStreamContract
		default:
			return core.ErrStreamContract
		}
	}
}

func writeStreamFailure(w http.ResponseWriter, err error) error {
	var gatewayErr *core.GatewayError
	if errors.As(err, &gatewayErr) {
		return writeError(w, gatewayErr)
	}
	return writeError(w, &core.GatewayError{Code: "upstream_stream_error", Category: core.CategoryUnavailable, Message: "Upstream stream failed"})
}
func copyHeaders(dst http.Header, src map[string][]string) {
	blocked := map[string]bool{"connection": true, "keep-alive": true, "proxy-connection": true, "proxy-authenticate": true, "proxy-authorization": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true, "set-cookie": true, "authorization": true}
	for n, vs := range src {
		if strings.EqualFold(n, "Connection") {
			for _, v := range vs {
				for _, x := range strings.Split(v, ",") {
					blocked[strings.ToLower(strings.TrimSpace(x))] = true
				}
			}
		}
	}
	for n, vs := range src {
		if !blocked[strings.ToLower(n)] {
			k := http.CanonicalHeaderKey(n)
			dst.Del(k)
			for _, v := range vs {
				dst.Add(k, v)
			}
		}
	}
}
func writeError(w http.ResponseWriter, e *core.GatewayError) error {
	status := 500
	switch e.Category {
	case core.CategoryInvalidRequest, core.CategoryUnsupportedFeature:
		status = 400
	case core.CategoryUnauthenticated:
		status = 401
	case core.CategoryPermissionDenied:
		status = 403
	case core.CategoryRateLimited:
		status = 429
	case core.CategoryUnavailable:
		status = 503
	case core.CategoryTimeout:
		status = 504
	}
	body, err := json.Marshal(struct {
		Error struct {
			Message string             `json:"message"`
			Type    core.ErrorCategory `json:"type"`
			Code    string             `json:"code"`
		} `json:"error"`
	}{Error: struct {
		Message string             `json:"message"`
		Type    core.ErrorCategory `json:"type"`
		Code    string             `json:"code"`
	}{e.Message, e.Category, e.Code}})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	if e.RetryAfter != nil && *e.RetryAfter >= 0 {
		sec := int64(*e.RetryAfter / time.Second)
		if *e.RetryAfter%time.Second != 0 {
			sec++
		}
		w.Header().Set("Retry-After", strconv.FormatInt(sec, 10))
	}
	w.WriteHeader(status)
	n, err := w.Write(body)
	if err == nil && n != len(body) {
		return io.ErrShortWrite
	}
	return err
}

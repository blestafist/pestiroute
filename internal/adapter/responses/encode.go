package responses

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

// Encode consumes one native response. Once Head is written, errors terminate
// the response without replacing the status or adding protocol bytes.
func Encode(w http.ResponseWriter, r *http.Request, resp core.ExecutionResponse, gatewayErr *core.GatewayError) error {
	if resp.Stream != nil {
		defer resp.Stream.Close()
	}
	if gatewayErr != nil {
		return encodeError(w, gatewayErr)
	}
	if resp.Stream == nil || r == nil {
		return core.ErrStreamContract
	}
	committed := false
	for {
		if err := r.Context().Err(); err != nil {
			return err
		}
		frame, err := resp.Stream.Next(r.Context())
		if err != nil {
			var gatewayError *core.GatewayError
			if !committed && r.Context().Err() == nil && errors.As(err, &gatewayError) {
				return encodeError(w, gatewayError)
			}
			if errors.Is(err, io.EOF) {
				return core.ErrStreamContract
			}
			return err
		}
		switch frame.Type {
		case core.FrameHead:
			if committed || frame.Head == nil || frame.Head.HTTPStatus == nil || *frame.Head.HTTPStatus < 200 || *frame.Head.HTTPStatus > 599 {
				return core.ErrStreamContract
			}
			copyResponseHeaders(w.Header(), frame.Head.Headers)
			if frame.Head.ContentType != "" {
				w.Header().Set("Content-Type", frame.Head.ContentType)
			}
			w.WriteHeader(*frame.Head.HTTPStatus)
			committed = true
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case core.FrameBody:
			if !committed || frame.Body == nil {
				return core.ErrStreamContract
			}
			n, err := w.Write(frame.Body.Data)
			if err != nil {
				return err
			}
			if n != len(frame.Body.Data) {
				return io.ErrShortWrite
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case core.FrameComplete:
			if !committed || frame.Complete == nil {
				return core.ErrStreamContract
			}
			// Complete is control metadata; require EOF without another write.
			_, err := resp.Stream.Next(r.Context())
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			return core.ErrStreamContract
		default:
			return core.ErrStreamContract
		}
	}
}

func copyResponseHeaders(dst http.Header, src map[string][]string) {
	blocked := map[string]bool{"connection": true, "keep-alive": true, "proxy-connection": true, "proxy-authenticate": true, "proxy-authorization": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true, "set-cookie": true, "authorization": true}
	for name, values := range src {
		if strings.EqualFold(name, "Connection") {
			for _, value := range values {
				for _, token := range strings.Split(value, ",") {
					blocked[strings.ToLower(strings.TrimSpace(token))] = true
				}
			}
		}
	}
	for name, values := range src {
		if blocked[strings.ToLower(name)] {
			continue
		}
		canonical := http.CanonicalHeaderKey(name)
		dst.Del(canonical)
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func encodeError(w http.ResponseWriter, e *core.GatewayError) error {
	status := http.StatusInternalServerError
	switch e.Category {
	case core.CategoryInvalidRequest, core.CategoryUnsupportedFeature:
		status = http.StatusBadRequest
	case core.CategoryUnauthenticated:
		status = http.StatusUnauthorized
	case core.CategoryPermissionDenied:
		status = http.StatusForbidden
	case core.CategoryRateLimited:
		status = http.StatusTooManyRequests
	case core.CategoryUnavailable:
		status = http.StatusServiceUnavailable
	case core.CategoryTimeout:
		status = http.StatusGatewayTimeout
	}
	type clientError struct {
		Message string             `json:"message"`
		Type    core.ErrorCategory `json:"type"`
		Code    string             `json:"code"`
	}
	body, err := json.Marshal(struct {
		Error clientError `json:"error"`
	}{clientError{e.Message, e.Category, e.Code}})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	if e.RetryAfter != nil && *e.RetryAfter >= 0 {
		seconds := int64(*e.RetryAfter / time.Second)
		if *e.RetryAfter%time.Second != 0 {
			seconds++
		}
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	w.WriteHeader(status)
	n, err := w.Write(body)
	if err == nil && n != len(body) {
		return io.ErrShortWrite
	}
	return err
}

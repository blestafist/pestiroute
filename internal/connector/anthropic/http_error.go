package anthropic

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const maxHTTPErrorBody = 64 << 10

func classifyHTTPRejection(resp *http.Response, now time.Time) *core.GatewayError {
	if resp.Body != nil {
		// Consume only a bounded prefix; provider error JSON is never interpreted or exposed.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxHTTPErrorBody))
		_ = resp.Body.Close()
	}

	status := resp.StatusCode
	category := core.CategoryUnavailable
	code := "upstream_rejection"
	message := "Upstream rejected request (HTTP " + strconv.Itoa(status) + ")"
	if status < http.StatusOK || status > 599 {
		code = "upstream_invalid_status"
		message = "Upstream returned an invalid HTTP status"
	} else if status >= 300 && status < 400 {
		code = "upstream_redirect"
		message = "Upstream redirect is not permitted"
	} else if status >= 200 && status < 300 {
		code = "upstream_invalid_status"
		message = "Upstream returned an unexpected successful HTTP status"
	}
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		category = core.CategoryInvalidRequest
	case http.StatusUnauthorized:
		category = core.CategoryUnauthenticated
	case http.StatusForbidden:
		category = core.CategoryPermissionDenied
	case http.StatusTooManyRequests:
		category = core.CategoryRateLimited
	default:
		if status >= 400 && status <= 499 {
			category = core.CategoryInvalidRequest
		}
	}
	err := &core.GatewayError{
		Code: code, Category: category, Message: message, RetryDisposition: core.RetryUnknown,
	}
	if status == http.StatusTooManyRequests || status >= 500 && status <= 599 {
		err.Retryable = true
		// A response status does not prove safe replay; disposition deliberately stays unknown.
		err.RetryAfter = parseHTTPRetryAfter(resp.Header.Get("Retry-After"), now)
	}
	return err
}

func parseHTTPRetryAfter(value string, now time.Time) *time.Duration {
	const maximum = 24 * time.Hour
	if value != "" && strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return nil
		}
		d := time.Duration(seconds) * time.Second
		if d <= maximum {
			return &d
		}
		return nil
	}
	at, err := http.ParseTime(value)
	if err != nil || !at.After(now) {
		return nil
	}
	d := at.Sub(now)
	if d > maximum {
		return nil
	}
	return &d
}

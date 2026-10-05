package codex

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

var errRequestHeaders = errors.New("Codex request headers are unavailable")

// buildRequestHeaders uses only the selected account's runtime credential and
// explicitly allowlisted client metadata. The returned headers own the bearer
// token for the duration of the upstream invocation; services are not retained.
func buildRequestHeaders(ctx context.Context, inbound map[string][]string, services core.InvocationServices, accountID string) (http.Header, error) {
	if services.Credentials == nil || accountID == "" {
		return nil, errRequestHeaders
	}
	credential, err := services.Credentials.Get(ctx, "oauth")
	if err != nil {
		return nil, errRequestHeaders
	}
	bundle, err := decodeOAuthBundle(credential)
	if err != nil || bundle.AccountID != accountID || !bundle.ExpiresAt.After(time.Now()) || !safeHeaderValue(bundle.AccessToken) || !safeHeaderValue(bundle.AccountID) {
		return nil, errRequestHeaders
	}

	blocked := map[string]bool{}
	for name, values := range inbound {
		if strings.EqualFold(name, "Connection") {
			for _, value := range values {
				for field := range strings.SplitSeq(value, ",") {
					blocked[strings.ToLower(strings.TrimSpace(field))] = true
				}
			}
		}
	}

	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+bundle.AccessToken)
	headers.Set("ChatGPT-Account-Id", bundle.AccountID)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	for name, values := range inbound {
		canonical := http.CanonicalHeaderKey(name)
		if blocked[strings.ToLower(name)] || (canonical != "Traceparent" && canonical != "Tracestate" && canonical != "User-Agent") {
			continue
		}
		if canonical == "User-Agent" && (len(values) != 1 || len(values[0]) > 512 || !safeHeaderValue(values[0])) {
			continue
		}
		limit := 512
		if canonical == "Traceparent" {
			limit = 256
		}
		for _, value := range values {
			if len(value) > limit || !safeHeaderValue(value) {
				continue
			}
			headers[canonical] = append(headers[canonical], value)
		}
	}
	return headers, nil
}

func safeHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

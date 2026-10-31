package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

const availableModelsURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"

const maxAvailableModelsResponse = 256 * 1024

var pinnedAvailableModelIDs = []string{
	"claude-opus-4-6-thinking", "claude-opus-5-5", "claude-opus-5-5-high", "claude-opus-5-5-low", "claude-opus-5-5-medium",
	"claude-sonnet-4-6", "claude-sonnet-5-5", "claude-sonnet-5-5-high", "claude-sonnet-5-5-low", "claude-sonnet-5-5-medium",
	"gemini-2.5-flash", "gemini-2.5-pro", "gemini-3.1-flash-image", "gemini-3.1-pro-high", "gemini-3.1-pro-low", "gemini-3.6-flash-high",
	"gemini-3.6-flash-low", "gemini-3.6-flash-medium", "gemini-3.7-flash-high", "gemini-3.7-flash-low", "gemini-3.7-flash-medium",
	"gemini-3.8-flash", "gemini-3.8-flash-high", "gemini-3.8-flash-low", "gemini-3.8-flash-medium", "gemini-4-argon-high",
	"gemini-pro-agent", "gpt-oss-120b-medium",
}

type availableModelsResult struct {
	Status       int             `json:"status"`
	Models       map[string]bool `json:"models"`
	UnknownCount int             `json:"unknown_count"`
}

var errAvailableModels = errors.New("Antigravity model availability probe failed")

// fetchAvailableModels performs one bounded metadata dispatch through the
// redirect-disabled runtime transport and durable budget wrapper.
func (c *Connector) fetchAvailableModels(ctx context.Context, doer core.HTTPDoer, budget core.HTTPRequestBudget, accountID, accessToken, project string, expiresAt time.Time) (availableModelsResult, error) {
	if ctx.Err() != nil || doer == nil || budget == nil || strings.TrimSpace(accountID) == "" || strings.TrimSpace(accessToken) == "" || strings.ContainsAny(accessToken, "\r\n") || !projectPattern.MatchString(project) || !expiresAt.After(time.Now()) {
		return availableModelsResult{}, errAvailableModels
	}
	body, err := json.Marshal(struct {
		Project string `json:"project"`
	}{project})
	if err != nil {
		return availableModelsResult{}, errAvailableModels
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, availableModelsURL, bytes.NewReader(body))
	if err != nil {
		return availableModelsResult{}, errAvailableModels
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", antigravityAuthUserAgent)
	req.Header.Set("X-Client-Name", "antigravity")
	req.Header.Set("X-Client-Version", "2.22.0")
	resp, err := (core.BudgetedHTTPDoer{Next: doer, Budget: budget, Account: accountID}).Do(req)
	if err != nil || resp == nil || resp.Body == nil {
		closeResponse(resp)
		return availableModelsResult{}, errAvailableModels
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return availableModelsResult{Status: resp.StatusCode}, errAvailableModels
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAvailableModelsResponse+1))
	if err != nil || len(data) > maxAvailableModelsResponse {
		return availableModelsResult{Status: resp.StatusCode}, errAvailableModels
	}
	result, err := parseAvailableModels(data)
	result.Status = resp.StatusCode
	if err != nil {
		return availableModelsResult{Status: resp.StatusCode}, errAvailableModels
	}
	return result, nil
}

func parseAvailableModels(data []byte) (availableModelsResult, error) {
	if len(data) == 0 || len(data) > maxAvailableModelsResponse || !uniqueJSON(data) {
		return availableModelsResult{}, errAvailableModels
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || envelope == nil {
		return availableModelsResult{}, errAvailableModels
	}
	var models map[string]json.RawMessage
	if json.Unmarshal(envelope["models"], &models) != nil || models == nil || len(models) > 200 {
		return availableModelsResult{}, errAvailableModels
	}
	known := make(map[string]bool, len(pinnedAvailableModelIDs))
	for _, id := range pinnedAvailableModelIDs {
		known[id] = false
	}
	unknown := 0
	for id := range models {
		var metadata map[string]json.RawMessage
		if json.Unmarshal(models[id], &metadata) != nil || metadata == nil {
			return availableModelsResult{}, errAvailableModels
		}
		if _, ok := known[id]; ok {
			known[id] = true
		} else {
			unknown++
		}
	}
	if unknown > 200 {
		return availableModelsResult{}, errAvailableModels
	}
	return availableModelsResult{Models: known, UnknownCount: unknown}, nil
}

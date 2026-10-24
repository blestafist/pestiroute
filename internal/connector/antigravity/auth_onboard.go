package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/blestafist/pestiroute/internal/core"
)

var operationPathPattern = regexp.MustCompile(`^(?:v1internal/)?operations/[a-zA-Z0-9_-]+$`)
var onboardProjectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
var operationPollInterval = 2 * time.Second

func tierBool(fields map[string]json.RawMessage, key string) (bool, bool) {
	raw, ok := fields[key]
	if !ok {
		return false, true
	}
	if string(raw) == "null" {
		return false, false
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	return value, true
}

func tierBoolNullable(fields map[string]json.RawMessage, key string) (bool, bool) {
	raw, ok := fields[key]
	if !ok || string(raw) == "null" {
		return false, true
	}
	return tierBool(fields, key)
}

func safeTierClass(id string) string {
	switch id {
	case "free-tier":
		return "free"
	case "legacy-tier":
		return "legacy"
	case "standard-tier":
		return "standard"
	default:
		return "other"
	}
}

func tierCandidateClass(data json.RawMessage) string {
	var root map[string]json.RawMessage
	var tiers []json.RawMessage
	if json.Unmarshal(data, &root) != nil || json.Unmarshal(root["allowedTiers"], &tiers) != nil {
		return "other"
	}
	for _, raw := range tiers {
		var tier map[string]json.RawMessage
		var id string
		if json.Unmarshal(raw, &tier) == nil && json.Unmarshal(tier["id"], &id) == nil && id == "free-tier" {
			return safeTierClass(id)
		}
	}
	return "other"
}

func discoveredTierClasses(data json.RawMessage) []string {
	var root map[string]json.RawMessage
	var tiers []json.RawMessage
	if json.Unmarshal(data, &root) != nil || json.Unmarshal(root["allowedTiers"], &tiers) != nil {
		return nil
	}
	classes := make([]string, 0, min(len(tiers), 1000))
	for _, raw := range tiers {
		var tier map[string]json.RawMessage
		var id string
		if json.Unmarshal(raw, &tier) != nil || json.Unmarshal(tier["id"], &id) != nil {
			continue
		}
		classes = append(classes, safeTierClass(id))
		if len(classes) == 1000 {
			break
		}
	}
	return classes
}

func eligibleFreeTier(data json.RawMessage) bool {
	if !uniqueJSON(data) {
		return false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	for _, key := range []string{"currentTier", "paidTier"} {
		if raw, ok := root[key]; ok && string(raw) != "null" {
			return false
		}
	}
	var tiersRaw []json.RawMessage
	if raw := root["allowedTiers"]; len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &tiersRaw) != nil {
		return false
	}
	var candidate map[string]json.RawMessage
	count := 0
	for _, raw := range tiersRaw {
		var tier map[string]json.RawMessage
		if json.Unmarshal(raw, &tier) != nil {
			return false
		}
		for _, key := range []string{"isDefault", "hasAcceptedTos", "userDefinedCloudaicompanionProject"} {
			var ok bool
			if key == "userDefinedCloudaicompanionProject" {
				_, ok = tierBoolNullable(tier, key)
			} else {
				_, ok = tierBool(tier, key)
			}
			if !ok {
				return false
			}
		}
		if privacyRaw := tier["privacyNotice"]; len(privacyRaw) != 0 && string(privacyRaw) != "null" {
			var privacy map[string]json.RawMessage
			if json.Unmarshal(privacyRaw, &privacy) != nil {
				return false
			}
			if _, ok := tierBool(privacy, "showNotice"); !ok {
				return false
			}
		}
		var id string
		if json.Unmarshal(tier["id"], &id) != nil {
			return false
		}
		if id == "free-tier" {
			candidate = tier
			count++
		}
	}
	if count != 1 {
		return false
	}
	for key, value := range candidate {
		switch key {
		case "isDefault", "hasAcceptedTos", "userDefinedCloudaicompanionProject":
			var ok bool
			if key == "userDefinedCloudaicompanionProject" {
				_, ok = tierBoolNullable(candidate, key)
			} else {
				_, ok = tierBool(candidate, key)
			}
			if !ok {
				return false
			}
		case "privacyNotice":
			if string(value) == "null" {
				continue
			}
			var privacy map[string]json.RawMessage
			if json.Unmarshal(value, &privacy) != nil {
				return false
			}
			show, ok := tierBool(privacy, "showNotice")
			if !ok || show {
				return false
			}
		}
	}
	userProject, ok := tierBoolNullable(candidate, "userDefinedCloudaicompanionProject")
	if !ok || userProject {
		return false
	}
	accepted, ok := tierBool(candidate, "hasAcceptedTos")
	if !ok || !accepted && len(candidate["hasAcceptedTos"]) != 0 {
		return false
	}
	if listedIneligible(root, "free-tier") {
		return false
	}
	return true
}

// tierEligibilityDiagnostic returns only fixed classifications and bounded
// structural telemetry. It does not participate in the eligibility decision.
func tierEligibilityDiagnostic(data json.RawMessage) (string, int, int, bool, bool, bool, bool) {
	if !uniqueJSON(data) {
		return "schema_invalid", 0, 0, false, false, false, false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return "schema_invalid", 0, 0, false, false, false, false
	}
	for _, key := range []string{"currentTier", "paidTier"} {
		if raw, ok := root[key]; ok && string(raw) != "null" {
			if key == "paidTier" {
				return "paid_present", 0, 0, false, false, false, false
			}
			return "auth_tier_ineligible", 0, 0, false, false, false, false
		}
	}
	var tiers []json.RawMessage
	if raw := root["allowedTiers"]; len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &tiers) != nil {
		return "schema_invalid", 0, 0, false, false, false, false
	}
	total := min(len(tiers), 1000)
	candidateCount, malformed := 0, false
	candidateSchemaInvalid, unrelatedSchemaInvalid := false, false
	candidateReason := ""
	for _, raw := range tiers {
		var tier map[string]json.RawMessage
		if json.Unmarshal(raw, &tier) != nil || tier == nil {
			malformed = true
			unrelatedSchemaInvalid = true
			continue
		}
		var id string
		if json.Unmarshal(tier["id"], &id) != nil {
			malformed = true
			unrelatedSchemaInvalid = true
			continue
		}
		isCandidate := id == "free-tier"
		if isCandidate {
			candidateCount++
		}
		invalid := false
		for _, key := range []string{"isDefault", "hasAcceptedTos", "userDefinedCloudaicompanionProject"} {
			var ok bool
			if key == "userDefinedCloudaicompanionProject" {
				_, ok = tierBoolNullable(tier, key)
			} else {
				_, ok = tierBool(tier, key)
			}
			if !ok {
				invalid = true
			}
		}
		if isCandidate {
			candidateSchemaInvalid = candidateSchemaInvalid || invalid
		}
		if !isCandidate && invalid {
			unrelatedSchemaInvalid = true
		}
		if isCandidate {
			if accepted, ok := tierBool(tier, "hasAcceptedTos"); ok && !accepted && len(tier["hasAcceptedTos"]) != 0 {
				if candidateReason == "" {
					candidateReason = "terms"
				}
			}
			if required, ok := tierBoolNullable(tier, "userDefinedCloudaicompanionProject"); ok && required {
				if candidateReason == "" {
					candidateReason = "userprojectrequired"
				}
			}
		}
		if pr := tier["privacyNotice"]; len(pr) > 0 && string(pr) != "null" {
			var p map[string]json.RawMessage
			if json.Unmarshal(pr, &p) != nil {
				if isCandidate {
					candidateSchemaInvalid = true
				} else {
					unrelatedSchemaInvalid = true
				}
			} else if show, ok := tierBool(p, "showNotice"); ok && show {
				if isCandidate {
					if candidateReason == "" {
						candidateReason = "privacy"
					}
				}
			} else if !ok {
				if isCandidate {
					candidateSchemaInvalid = true
				} else {
					unrelatedSchemaInvalid = true
				}
			}
		}
		malformed = candidateSchemaInvalid || unrelatedSchemaInvalid
	}
	if malformed {
		return "schema_invalid", total, min(candidateCount, 1000), false, true, candidateSchemaInvalid, unrelatedSchemaInvalid
	}
	if candidateCount == 0 {
		return "candidate_missing", total, 0, false, false, false, false
	}
	if candidateCount > 1 {
		return "ambiguous", total, min(candidateCount, 1000), false, false, false, false
	}
	if candidateReason != "" {
		return candidateReason, total, 1, false, false, false, false
	}
	if listedIneligible(root, "free-tier") {
		reason := "providerineligible"
		var entries []json.RawMessage
		_ = json.Unmarshal(root["ineligibleTiers"], &entries)
		for _, entryRaw := range entries {
			var entry map[string]json.RawMessage
			_ = json.Unmarshal(entryRaw, &entry)
			var code string
			if json.Unmarshal(entry["reasonCode"], &code) == nil && code == "VALIDATION_REQUIRED" {
				reason = "validationrequired"
			}
		}
		return reason, total, 1, true, false, false, false
	}
	return "auth_tier_ineligible", total, 1, false, false, false, false
}

func hasCurrentTier(data json.RawMessage) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	raw, ok := root["currentTier"]
	return ok && string(raw) != "null"
}

func listedIneligible(root map[string]json.RawMessage, candidate string) bool {
	raw := root["ineligibleTiers"]
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var tiers []json.RawMessage
	if json.Unmarshal(raw, &tiers) != nil {
		return true
	}
	for _, item := range tiers {
		var entry map[string]json.RawMessage
		if json.Unmarshal(item, &entry) != nil {
			return true
		}
		var id, reason string
		if json.Unmarshal(entry["tierId"], &id) != nil && len(entry["tierId"]) != 0 {
			return true
		}
		if json.Unmarshal(entry["reasonCode"], &reason) != nil && len(entry["reasonCode"]) != 0 {
			return true
		}
		if id == candidate || reason == "VALIDATION_REQUIRED" {
			return true
		}
	}
	return false
}

func onboardProject(ctx context.Context, origin string, transport core.HTTPDoer, token string, discovery json.RawMessage, logger *slog.Logger) (string, *core.GatewayError) {
	if hasCurrentTier(discovery) {
		return "", &core.GatewayError{Code: "auth_project_missing", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Authentication project data is missing"}
	}
	if !eligibleFreeTier(discovery) {
		reason, allowedCount, candidateCount, validationRequired, malformedEntry, candidateSchemaInvalid, unrelatedSchemaInvalid := tierEligibilityDiagnostic(discovery)
		if logger != nil {
			logger.Debug("authentication diagnostic", "reason", "auth_tier_"+reason, "tier_class", tierCandidateClass(discovery), "tier_classes", discoveredTierClasses(discovery), "allowed_count", allowedCount, "candidate_count", candidateCount, "validation_required", validationRequired, "malformed_entry", malformedEntry, "candidate_schema_invalid", candidateSchemaInvalid, "unrelated_schema_invalid", unrelatedSchemaInvalid)
		}
		code := "auth_tier_" + reason
		if reason == "auth_tier_ineligible" {
			code = reason
		}
		return "", tierIneligibleError(code)
	}
	if logger != nil {
		logger.Info("authentication diagnostic", "reason", "auth_onboarding_started")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body := []byte(`{"tierId":"free-tier","metadata":{"ideType":"IDE_UNSPECIFIED","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}}`)
	resp, ge := onboardingRequest(ctx, transport, http.MethodPost, origin+"/v1internal:onboardUser", token, body)
	if ge != nil {
		return "", ge
	}
	data, ge := onboardingResponse(ctx, resp)
	if ge != nil {
		return "", ge
	}
	var operation struct {
		Name     string          `json:"name"`
		Done     bool            `json:"done"`
		Error    json.RawMessage `json:"error"`
		Response json.RawMessage `json:"response"`
		Project  json.RawMessage `json:"cloudaicompanionProject"`
	}
	if !uniqueJSON(data) || json.Unmarshal(data, &operation) != nil {
		return "", onboardingFailedError()
	}
	if len(operation.Error) > 0 && string(operation.Error) != "null" {
		return "", onboardingFailedError()
	}
	if project := returnedProject(operation.Project, operation.Response); project != "" {
		return project, nil
	}
	if operation.Done {
		return "", onboardingFailedError()
	}
	if !validOperationName(operation.Name) {
		return "", onboardingFailedError()
	}
	path := operation.Name
	pollURL := strings.TrimRight(origin, "/") + "/" + path
	for i := 0; i < 12; i++ {
		timer := time.NewTimer(operationPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", contextError(ctx.Err())
		case <-timer.C:
		}
		if logger != nil {
			logger.Info("authentication diagnostic", "reason", "auth_onboarding_polling")
		}
		resp, ge = onboardingRequest(ctx, transport, http.MethodGet, pollURL, token, nil)
		if ge != nil {
			return "", ge
		}
		data, ge = onboardingResponse(ctx, resp)
		if ge != nil {
			return "", ge
		}
		operation = struct {
			Name     string          `json:"name"`
			Done     bool            `json:"done"`
			Error    json.RawMessage `json:"error"`
			Response json.RawMessage `json:"response"`
			Project  json.RawMessage `json:"cloudaicompanionProject"`
		}{}
		if !uniqueJSON(data) || json.Unmarshal(data, &operation) != nil {
			return "", onboardingFailedError()
		}
		if len(operation.Error) > 0 && string(operation.Error) != "null" {
			return "", onboardingFailedError()
		}
		if operation.Done {
			if project := returnedProject(operation.Project, operation.Response); project != "" {
				return project, nil
			}
			return "", onboardingFailedError()
		}
	}
	return "", onboardingFailedError()
}

func validOperationName(name string) bool {
	return operationPathPattern.MatchString(name)
}

func returnedProject(project, response json.RawMessage) string {
	for _, raw := range []json.RawMessage{project, response} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var envelope struct {
			Project json.RawMessage `json:"cloudaicompanionProject"`
		}
		if json.Unmarshal(raw, &envelope) == nil && len(envelope.Project) > 0 {
			raw = envelope.Project
		}
		var value struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &value) == nil && onboardProjectPattern.MatchString(value.ID) {
			return value.ID
		}
	}
	return ""
}

func onboardingRequest(ctx context.Context, transport core.HTTPDoer, method, endpoint, token string, body []byte) (*http.Response, *core.GatewayError) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, onboardingFailedError()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := transport.Do(req)
	if err != nil {
		closeResponse(resp)
		if errors.Is(err, core.ErrRequestBudgetExceeded) {
			return nil, requestBudgetError()
		}
		if ctx.Err() != nil {
			return nil, contextError(ctx.Err())
		}
		return nil, onboardingFailedError()
	}
	if resp == nil || resp.Body == nil {
		return nil, onboardingFailedError()
	}
	if resp.StatusCode != http.StatusOK {
		closeResponse(resp)
		return nil, onboardingFailedError()
	}
	return resp, nil
}

func onboardingResponse(ctx context.Context, resp *http.Response) ([]byte, *core.GatewayError) {
	data, err := readAuthResponse(ctx, resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, contextError(ctx.Err())
		}
		return nil, onboardingFailedError()
	}
	return data, nil
}

func tierIneligibleError(code string) *core.GatewayError {
	return &core.GatewayError{Code: code, Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Authentication tier is ineligible"}
}
func onboardingFailedError() *core.GatewayError {
	return &core.GatewayError{Code: "auth_onboarding_failed", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Authentication onboarding failed"}
}

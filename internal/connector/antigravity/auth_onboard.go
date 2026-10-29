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
		var isDefault bool
		if json.Unmarshal(raw, &tier) == nil && json.Unmarshal(tier["id"], &id) == nil && (id == "free-tier" || id == "standard-tier") && json.Unmarshal(tier["isDefault"], &isDefault) == nil && isDefault {
			return safeTierClass(id)
		}
	}
	return "other"
}

func tierEligibilityFlags(data json.RawMessage) (bool, bool, bool, bool, bool, bool, bool) {
	var root map[string]json.RawMessage
	var tiers []json.RawMessage
	if json.Unmarshal(data, &root) != nil || json.Unmarshal(root["allowedTiers"], &tiers) != nil {
		return false, false, false, false, false, false, false
	}
	var foundDefault, userProject, terms, privacyRequired bool
	var defaultCandidate string
	defaultCount := 0
	aggregateIneligible := listedIneligible(root, "free-tier") || listedIneligible(root, "standard-tier")
	validationRequired := ineligibleReason(root, "free-tier") == "validationrequired" || ineligibleReason(root, "standard-tier") == "validationrequired"
	for _, raw := range tiers[:min(len(tiers), 1000)] {
		var tier map[string]json.RawMessage
		var id string
		if json.Unmarshal(raw, &tier) != nil || json.Unmarshal(tier["id"], &id) != nil || (id != "free-tier" && id != "standard-tier") {
			continue
		}
		if value, ok := tierBool(tier, "isDefault"); ok && value {
			foundDefault = true
			defaultCount++
			defaultCandidate = id
		}
		if value, ok := tierBoolNullable(tier, "userDefinedCloudaicompanionProject"); ok && value {
			userProject = true
		}
		if value, ok := tierBool(tier, "hasAcceptedTos"); ok && !value && len(tier["hasAcceptedTos"]) > 0 {
			terms = true
		}
		var privacy map[string]json.RawMessage
		if json.Unmarshal(tier["privacyNotice"], &privacy) == nil {
			if value, ok := tierBoolNullable(privacy, "showNotice"); ok && value {
				privacyRequired = true
			}
		}
	}
	if defaultCount != 1 {
		defaultCandidate = ""
	}
	candidateIneligible := defaultCandidate != "" && listedIneligible(root, defaultCandidate)
	return foundDefault, userProject, terms, privacyRequired, candidateIneligible, aggregateIneligible, validationRequired
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

// analyzeOnboardingTier evaluates the complete bounded discovery shape before
// selecting an explicitly advertised managed candidate.
func analyzeOnboardingTier(data json.RawMessage) (string, string, int, int, bool, bool, bool) {
	if !uniqueJSON(data) {
		return "", "schema_invalid", 0, 0, false, true, false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return "", "schema_invalid", 0, 0, false, true, false
	}
	for _, key := range []string{"currentTier", "paidTier"} {
		if raw, ok := root[key]; ok && string(raw) != "null" {
			if key == "paidTier" {
				return "", "paid_present", 0, 0, false, false, false
			}
			return "", "auth_tier_ineligible", 0, 0, false, false, false
		}
	}
	var tiersRaw []json.RawMessage
	if raw := root["allowedTiers"]; len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &tiersRaw) != nil {
		return "", "schema_invalid", 0, 0, false, true, false
	}
	total := min(len(tiersRaw), 1000)
	type tierInfo struct {
		id        string
		isDefault bool
		reason    string
	}
	var candidates []tierInfo
	counts := map[string]int{}
	defaultCount := 0
	malformed, candidateMalformed, unrelatedMalformed := false, false, false
	for _, raw := range tiersRaw {
		var tier map[string]json.RawMessage
		if json.Unmarshal(raw, &tier) != nil || tier == nil {
			malformed, unrelatedMalformed = true, true
			continue
		}
		var id string
		if len(tier["id"]) == 0 || string(tier["id"]) == "null" || json.Unmarshal(tier["id"], &id) != nil {
			malformed, unrelatedMalformed = true, true
			continue
		}
		candidate := id == "free-tier" || id == "standard-tier"
		isDefault, defaultOK := tierBool(tier, "isDefault")
		accepted, termsOK := tierBool(tier, "hasAcceptedTos")
		project, projectOK := tierBoolNullable(tier, "userDefinedCloudaicompanionProject")
		invalid := !defaultOK || !termsOK || !projectOK
		privacy := false
		if pr := tier["privacyNotice"]; len(pr) != 0 && string(pr) != "null" {
			var p map[string]json.RawMessage
			if json.Unmarshal(pr, &p) != nil || p == nil {
				invalid = true
			} else {
				var ok bool
				privacy, ok = tierBoolNullable(p, "showNotice")
				invalid = invalid || !ok
			}
		}
		if invalid {
			malformed = true
			if candidate {
				candidateMalformed = true
			} else {
				unrelatedMalformed = true
			}
		}
		if isDefault && defaultOK {
			defaultCount++
		}
		if candidate {
			counts[id]++
			reason := ""
			if !accepted && len(tier["hasAcceptedTos"]) > 0 && termsOK {
				reason = "terms"
			}
			if privacy && reason == "" {
				reason = "privacy"
			}
			if project && reason == "" {
				reason = "userprojectrequired"
			}
			candidates = append(candidates, tierInfo{id: id, isDefault: isDefault && defaultOK, reason: reason})
		}
	}
	if !ineligibleSchemaValid(root) {
		malformed = true
		unrelatedMalformed = true
	}
	if malformed {
		return "", "schema_invalid", total, len(candidates), false, candidateMalformed, unrelatedMalformed
	}
	for _, n := range counts {
		if n > 1 {
			return "", "ambiguous", total, len(candidates), false, false, false
		}
	}
	if defaultCount > 1 {
		return "", "ambiguous", total, len(candidates), false, false, false
	}
	if defaultCount == 0 {
		return "", "candidate_missing", total, len(candidates), false, false, false
	}
	var selected tierInfo
	selectedFound := false
	for _, candidate := range candidates {
		if candidate.isDefault {
			selected = candidate
			selectedFound = true
			break
		}
	}
	if !selectedFound {
		return "", "candidate_missing", total, len(candidates), false, false, false
	}
	ineligible := listedIneligible(root, selected.id)
	ineligibleReasonCode := ""
	if ineligible {
		ineligibleReasonCode = ineligibleReason(root, selected.id)
	}
	if selected.reason != "" {
		return "", selected.reason, total, len(candidates), ineligibleReasonCode == "validationrequired", false, false
	}
	if ineligible {
		return "", ineligibleReasonCode, total, len(candidates), ineligibleReasonCode == "validationrequired", false, false
	}
	return selected.id, "", total, len(candidates), false, false, false
}

func ineligibleSchemaValid(root map[string]json.RawMessage) bool {
	raw := root["ineligibleTiers"]
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return false
	}
	for _, raw := range entries {
		var entry map[string]json.RawMessage
		if json.Unmarshal(raw, &entry) != nil || entry == nil {
			return false
		}
		for _, key := range []string{"tierId", "reasonCode"} {
			if value, exists := entry[key]; exists && string(value) != "null" {
				var text string
				if json.Unmarshal(value, &text) != nil {
					return false
				}
			}
		}
	}
	return true
}

// tierEligibilityDiagnostic returns only fixed classifications and bounded
// structural telemetry. It does not participate in the eligibility decision.
func tierEligibilityDiagnostic(data json.RawMessage) (string, int, int, bool, bool, bool, bool) {
	_, reason, total, candidates, validationRequired, candidateBad, unrelatedBad := analyzeOnboardingTier(data)
	malformed := candidateBad || unrelatedBad
	return reason, total, candidates, validationRequired, malformed, candidateBad, unrelatedBad
}

func ineligibleReason(root map[string]json.RawMessage, candidate string) string {
	var entries []json.RawMessage
	_ = json.Unmarshal(root["ineligibleTiers"], &entries)
	for _, raw := range entries {
		var entry map[string]json.RawMessage
		_ = json.Unmarshal(raw, &entry)
		var id, code string
		_ = json.Unmarshal(entry["tierId"], &id)
		_ = json.Unmarshal(entry["reasonCode"], &code)
		if id == candidate {
			switch code {
			case "VALIDATION_REQUIRED":
				return "validationrequired"
			case "UNSUPPORTED_CLIENT":
				return "unsupportedclient"
			}
		}
	}
	return "providerineligible"
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
		if id == candidate {
			return true
		}
	}
	return false
}

func onboardProject(ctx context.Context, origin string, transport core.HTTPDoer, token string, discovery json.RawMessage, logger *slog.Logger) (string, *core.GatewayError) {
	if hasCurrentTier(discovery) {
		if logger != nil {
			defaultFlag, userProjectFlag, termsFlag, privacyFlag, ineligibleFlag, aggregateIneligibleFlag, validationRequired := tierEligibilityFlags(discovery)
			logger.Debug("authentication diagnostic", "reason", "auth_project_missing", "tier_class", tierCandidateClass(discovery), "tier_classes", discoveredTierClasses(discovery), "default", defaultFlag, "userproject", userProjectFlag, "terms", termsFlag, "privacy", privacyFlag, "ineligible", ineligibleFlag, "aggregate_ineligible", aggregateIneligibleFlag, "validation_required", validationRequired)
		}
		return "", &core.GatewayError{Code: "auth_project_missing", Category: core.CategoryUnavailable, RetryDisposition: core.RetryUnknown, Message: "Authentication project data is missing"}
	}
	selected, eligibilityReason, _, _, _, _, _ := analyzeOnboardingTier(discovery)
	if selected == "" || eligibilityReason != "" {
		reason, allowedCount, candidateCount, validationRequired, malformedEntry, candidateSchemaInvalid, unrelatedSchemaInvalid := tierEligibilityDiagnostic(discovery)
		if logger != nil {
			defaultFlag, userProjectFlag, termsFlag, privacyFlag, ineligibleFlag, aggregateIneligibleFlag, _ := tierEligibilityFlags(discovery)
			logger.Debug("authentication diagnostic", "reason", "auth_tier_"+reason, "tier_class", tierCandidateClass(discovery), "tier_classes", discoveredTierClasses(discovery), "allowed_count", allowedCount, "candidate_count", candidateCount, "default", defaultFlag, "userproject", userProjectFlag, "terms", termsFlag, "privacy", privacyFlag, "ineligible", ineligibleFlag, "aggregate_ineligible", aggregateIneligibleFlag, "validation_required", validationRequired, "malformed_entry", malformedEntry, "candidate_schema_invalid", candidateSchemaInvalid, "unrelated_schema_invalid", unrelatedSchemaInvalid)
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
	body, _ := json.Marshal(struct {
		TierID   string `json:"tierId"`
		Metadata struct {
			IDEType    int `json:"ideType"`
			Platform   int `json:"platform"`
			PluginType int `json:"pluginType"`
		} `json:"metadata"`
	}{TierID: selected, Metadata: struct {
		IDEType    int `json:"ideType"`
		Platform   int `json:"platform"`
		PluginType int `json:"pluginType"`
	}{IDEType: 9, Platform: 3, PluginType: 2}})
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
		if method == http.MethodPost {
			req.Header.Set("User-Agent", antigravityAuthUserAgent)
			req.Header.Set("X-Request-Source", antigravityRequestSource)
		}
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

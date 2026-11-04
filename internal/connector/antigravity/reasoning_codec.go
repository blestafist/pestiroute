package antigravity

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"
)

const (
	reasoningCarrierLimit = 1 << 20
	reasoningSuite        = "AES-256-GCM"
	reasoningKeyVersion   = "current-master/v1"
)

var (
	ErrReasoningCarrier          = errors.New("invalid encrypted reasoning continuation")
	ErrContinuationScopeMismatch = errors.New("continuation_scope_mismatch")
)

type reasoningScope struct{ AccountID, ProjectID, Model string }

type ReasoningCodec struct {
	aead  cipher.AEAD
	clock func() time.Time
}

func NewReasoningCodec(key [32]byte, clock func() time.Time) (*ReasoningCodec, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, ErrReasoningCarrier
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrReasoningCarrier
	}
	if clock == nil {
		clock = time.Now
	}
	return &ReasoningCodec{aead: aead, clock: clock}, nil
}

type reasoningFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
	ID   *string         `json:"id"`
}
type reasoningSource struct {
	CandidateIndex    int                    `json:"candidate_index"`
	PartIndex         int                    `json:"part_index"`
	PartKind          string                 `json:"part_kind"`
	ProviderPartID    *string                `json:"provider_part_id"`
	SignatureBytesB64 string                 `json:"signature_bytes_b64"`
	Content           *string                `json:"content"`
	FunctionCall      *reasoningFunctionCall `json:"function_call"`
}
type reasoningAssociation struct {
	Kind               string `json:"kind"`
	SignaturePartIndex int    `json:"signature_part_index"`
	SourcePartIndex    int    `json:"source_part_index"`
}
type reasoningResponses struct {
	ReasoningItemID  string  `json:"reasoning_item_id"`
	OutputIndex      int     `json:"output_index"`
	AssociatedItemID *string `json:"associated_item_id"`
	AssociatedCallID *string `json:"associated_call_id"`
}
type reasoningPlaintext struct {
	SchemaVersion int                  `json:"schema_version"`
	Source        reasoningSource      `json:"source"`
	Association   reasoningAssociation `json:"association"`
	Responses     reasoningResponses   `json:"responses"`
}
type reasoningCarrier struct {
	FormatVersion      int     `json:"format_version"`
	KeyVersion         string  `json:"key_version"`
	Cryptosuite        string  `json:"cryptosuite"`
	IssuedAt           int64   `json:"issued_at"`
	ExpiresAt          int64   `json:"expires_at"`
	AccountID          string  `json:"account_id"`
	ProjectID          string  `json:"project_id"`
	Model              string  `json:"model"`
	CandidateIndex     int     `json:"candidate_index"`
	PartIndex          int     `json:"part_index"`
	PartKind           string  `json:"part_kind"`
	ProviderPartID     *string `json:"provider_part_id"`
	AssociationKind    string  `json:"association_kind"`
	SignaturePartIndex int     `json:"signature_part_index"`
	SourcePartIndex    int     `json:"source_part_index"`
	OutputIndex        int     `json:"output_index"`
	ReasoningItemID    string  `json:"reasoning_item_id"`
	AssociatedItemID   *string `json:"associated_item_id"`
	AssociatedCallID   *string `json:"associated_call_id"`
	Nonce              string  `json:"nonce"`
	Ciphertext         string  `json:"ciphertext"`
}

func (c *ReasoningCodec) seal(value reasoningPlaintext, scope reasoningScope, issuedAt, expiresAt time.Time) ([]byte, error) {
	now := time.Time{}
	if c != nil && c.clock != nil {
		now = c.clock()
	}
	if c == nil || c.aead == nil || !validReasoningScope(scope) || !validReasoningTimes(now, issuedAt, expiresAt) || !validateReasoningPlaintext(value) {
		return nil, ErrReasoningCarrier
	}
	plain, err := json.Marshal(value)
	if err != nil || len(plain) > reasoningCarrierLimit {
		return nil, ErrReasoningCarrier
	}
	carrier := carrierMetadata(value, scope, issuedAt.Unix(), expiresAt.Unix())
	aad, err := reasoningAAD(scope, carrier)
	if err != nil {
		return nil, ErrReasoningCarrier
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, ErrReasoningCarrier
	}
	carrier.Nonce = base64.RawStdEncoding.EncodeToString(nonce)
	carrier.Ciphertext = base64.RawStdEncoding.EncodeToString(c.aead.Seal(nil, nonce, plain, aad))
	raw, err := json.Marshal(carrier)
	if err != nil || len(raw) > reasoningCarrierLimit {
		return nil, ErrReasoningCarrier
	}
	return raw, nil
}

func (c *ReasoningCodec) open(raw []byte, scope reasoningScope) (reasoningPlaintext, error) {
	if c == nil || c.aead == nil || len(raw) == 0 || len(raw) > reasoningCarrierLimit || !validReasoningScope(scope) || !uniqueJSON(raw) {
		return reasoningPlaintext{}, ErrReasoningCarrier
	}
	var carrier reasoningCarrier
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&carrier) != nil || d.Decode(new(any)) != io.EOF || !requiredCarrierFields(raw) || carrier.FormatVersion != 1 || carrier.KeyVersion != reasoningKeyVersion || carrier.Cryptosuite != reasoningSuite {
		return reasoningPlaintext{}, ErrReasoningCarrier
	}
	if carrier.AccountID != scope.AccountID || carrier.ProjectID != scope.ProjectID || carrier.Model != scope.Model {
		return reasoningPlaintext{}, ErrContinuationScopeMismatch
	}
	if len(carrier.Ciphertext) > base64.RawStdEncoding.EncodedLen(reasoningCarrierLimit) {
		return reasoningPlaintext{}, ErrReasoningCarrier
	}
	nonce, err := base64.RawStdEncoding.Strict().DecodeString(carrier.Nonce)
	if err != nil || len(nonce) != c.aead.NonceSize() {
		return reasoningPlaintext{}, ErrReasoningCarrier
	}
	ciphertext, err := base64.RawStdEncoding.Strict().DecodeString(carrier.Ciphertext)
	if err != nil || len(ciphertext) < c.aead.Overhead() || len(ciphertext) > reasoningCarrierLimit {
		return reasoningPlaintext{}, ErrReasoningCarrier
	}
	now := c.clock()
	if !validReasoningTimes(now, time.Unix(carrier.IssuedAt, 0), time.Unix(carrier.ExpiresAt, 0)) {
		return reasoningPlaintext{}, ErrReasoningCarrier
	}
	aad, err := reasoningAAD(scope, carrier)
	if err != nil {
		return reasoningPlaintext{}, ErrReasoningCarrier
	}
	plain, err := c.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil || len(plain) > reasoningCarrierLimit || !uniqueJSON(plain) {
		return reasoningPlaintext{}, ErrReasoningCarrier
	}
	d = json.NewDecoder(bytes.NewReader(plain))
	d.DisallowUnknownFields()
	var value reasoningPlaintext
	if d.Decode(&value) != nil || d.Decode(new(any)) != io.EOF || !requiredReasoningFields(plain) || !validateReasoningPlaintext(value) || !carrierMatches(carrier, value) {
		return reasoningPlaintext{}, ErrReasoningCarrier
	}
	return value, nil
}

func validReasoningScope(s reasoningScope) bool {
	return s.AccountID != "" && s.ProjectID != "" && s.Model != "" && len(s.AccountID) <= 256 && len(s.ProjectID) <= 256 && len(s.Model) <= 256
}

func validReasoningTimes(now, issued, expires time.Time) bool {
	return expires.After(issued) && expires.Sub(issued) <= 24*time.Hour && !issued.After(now.Add(5*time.Minute)) && now.Before(expires)
}

func validateReasoningPlaintext(v reasoningPlaintext) bool {
	s := v.Source
	if !boundedReasoningValue(v) {
		return false
	}
	if v.SchemaVersion != 1 || s.CandidateIndex < 0 || s.PartIndex < 0 || v.Association.SignaturePartIndex < 0 || v.Association.SourcePartIndex != s.PartIndex || v.Association.SignaturePartIndex < v.Association.SourcePartIndex || v.Responses.OutputIndex < 0 || v.Responses.ReasoningItemID == "" || len(v.Responses.ReasoningItemID) > 256 {
		return false
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(s.SignatureBytesB64)
	if err != nil || len(signature) == 0 || len(s.SignatureBytesB64) > reasoningCarrierLimit {
		return false
	}
	for _, optional := range []*string{s.ProviderPartID, v.Responses.AssociatedItemID, v.Responses.AssociatedCallID} {
		if optional != nil && (len(*optional) == 0 || len(*optional) > 256) {
			return false
		}
	}
	switch s.PartKind {
	case "text", "thought":
		associationOK := v.Association.Kind == "direct" && v.Association.SignaturePartIndex == s.PartIndex || v.Association.Kind == "adjacent" && v.Association.SignaturePartIndex == s.PartIndex+1
		if s.PartKind == "text" {
			return s.Content != nil && *s.Content != "" && s.FunctionCall == nil && len(*s.Content) <= reasoningCarrierLimit && associationOK && v.Responses.AssociatedItemID != nil && v.Responses.AssociatedCallID == nil
		}
		return s.Content != nil && *s.Content != "" && s.FunctionCall == nil && len(*s.Content) <= reasoningCarrierLimit && associationOK && v.Responses.AssociatedItemID == nil && v.Responses.AssociatedCallID == nil
	case "function_call":
		associationOK := v.Association.Kind == "direct" && v.Association.SignaturePartIndex == s.PartIndex || v.Association.Kind == "adjacent" && v.Association.SignaturePartIndex == s.PartIndex+1
		return s.Content == nil && s.FunctionCall != nil && associationOK && v.Responses.AssociatedItemID == nil && v.Responses.AssociatedCallID != nil && s.FunctionCall.Name != "" && len(s.FunctionCall.Name) <= 256 && len(s.FunctionCall.Args) > 0 && len(s.FunctionCall.Args) <= reasoningCarrierLimit && uniqueJSON(s.FunctionCall.Args) && json.Valid(s.FunctionCall.Args)
	default:
		return false
	}
}

func boundedReasoningValue(v reasoningPlaintext) bool {
	remaining := reasoningCarrierLimit
	add := func(n int) bool {
		if n < 0 || n > remaining {
			return false
		}
		remaining -= n
		return true
	}
	for _, n := range []int{len(v.Source.SignatureBytesB64), len(v.Source.PartKind), len(v.Responses.ReasoningItemID), len(v.Association.Kind)} {
		if !add(n) {
			return false
		}
	}
	for _, p := range []*string{v.Source.ProviderPartID, v.Source.Content, v.Responses.AssociatedItemID, v.Responses.AssociatedCallID} {
		if p != nil && !add(len(*p)) {
			return false
		}
	}
	if v.Source.FunctionCall != nil && (!add(len(v.Source.FunctionCall.Name)) || !add(len(v.Source.FunctionCall.Args)) || v.Source.FunctionCall.ID != nil && !add(len(*v.Source.FunctionCall.ID))) {
		return false
	}
	return true
}

func requiredReasoningFields(raw []byte) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || !hasFields(root, "schema_version", "source", "association", "responses") {
		return false
	}
	var source, association, responses map[string]json.RawMessage
	if json.Unmarshal(root["source"], &source) != nil || json.Unmarshal(root["association"], &association) != nil || json.Unmarshal(root["responses"], &responses) != nil || !hasFields(source, "candidate_index", "part_index", "part_kind", "provider_part_id", "signature_bytes_b64", "content", "function_call") || !hasFields(association, "kind", "signature_part_index", "source_part_index") || !hasFields(responses, "reasoning_item_id", "output_index", "associated_item_id", "associated_call_id") {
		return false
	}
	if !bytes.Equal(bytes.TrimSpace(source["function_call"]), []byte("null")) {
		var call map[string]json.RawMessage
		if json.Unmarshal(source["function_call"], &call) != nil || !hasFields(call, "name", "args", "id") {
			return false
		}
	}
	return true
}

func hasFields(m map[string]json.RawMessage, fields ...string) bool {
	for _, field := range fields {
		if _, ok := m[field]; !ok {
			return false
		}
	}
	return true
}

func requiredCarrierFields(raw []byte) bool {
	var fields map[string]json.RawMessage
	return json.Unmarshal(raw, &fields) == nil && hasFields(fields, "format_version", "key_version", "cryptosuite", "issued_at", "expires_at", "account_id", "project_id", "model", "candidate_index", "part_index", "part_kind", "provider_part_id", "association_kind", "signature_part_index", "source_part_index", "output_index", "reasoning_item_id", "associated_item_id", "associated_call_id", "nonce", "ciphertext")
}

func carrierMetadata(v reasoningPlaintext, scope reasoningScope, issued, expires int64) reasoningCarrier {
	return reasoningCarrier{FormatVersion: 1, KeyVersion: reasoningKeyVersion, Cryptosuite: reasoningSuite, IssuedAt: issued, ExpiresAt: expires, AccountID: scope.AccountID, ProjectID: scope.ProjectID, Model: scope.Model, CandidateIndex: v.Source.CandidateIndex, PartIndex: v.Source.PartIndex, PartKind: v.Source.PartKind, ProviderPartID: v.Source.ProviderPartID, AssociationKind: v.Association.Kind, SignaturePartIndex: v.Association.SignaturePartIndex, SourcePartIndex: v.Association.SourcePartIndex, OutputIndex: v.Responses.OutputIndex, ReasoningItemID: v.Responses.ReasoningItemID, AssociatedItemID: v.Responses.AssociatedItemID, AssociatedCallID: v.Responses.AssociatedCallID}
}

func carrierMatches(c reasoningCarrier, v reasoningPlaintext) bool {
	return c.CandidateIndex == v.Source.CandidateIndex && c.PartIndex == v.Source.PartIndex && c.PartKind == v.Source.PartKind && optionalString(c.ProviderPartID) == optionalString(v.Source.ProviderPartID) && c.AssociationKind == v.Association.Kind && c.SignaturePartIndex == v.Association.SignaturePartIndex && c.SourcePartIndex == v.Association.SourcePartIndex && c.OutputIndex == v.Responses.OutputIndex && c.ReasoningItemID == v.Responses.ReasoningItemID && optionalString(c.AssociatedItemID) == optionalString(v.Responses.AssociatedItemID) && optionalString(c.AssociatedCallID) == optionalString(v.Responses.AssociatedCallID)
}

func reasoningAAD(scope reasoningScope, c reasoningCarrier) ([]byte, error) {
	fields := []string{"reasoning-continuity/v1", c.KeyVersion, c.Cryptosuite, scope.AccountID, scope.ProjectID, scope.Model, stringInt(c.IssuedAt), stringInt(c.ExpiresAt), stringInt(int64(c.CandidateIndex)), stringInt(int64(c.PartIndex)), c.PartKind, optionalString(c.ProviderPartID), c.AssociationKind, stringInt(int64(c.SignaturePartIndex)), stringInt(int64(c.SourcePartIndex)), c.ReasoningItemID, stringInt(int64(c.OutputIndex)), optionalString(c.AssociatedItemID), optionalString(c.AssociatedCallID)}
	var b bytes.Buffer
	for _, field := range fields {
		if len(field) > reasoningCarrierLimit || uint64(len(field)) > uint64(^uint32(0)) {
			return nil, ErrReasoningCarrier
		}
		_ = binary.Write(&b, binary.BigEndian, uint32(len(field)))
		_, _ = b.WriteString(field)
	}
	return b.Bytes(), nil
}

func stringInt(v int64) string { return strconv.FormatInt(v, 10) }
func optionalString(v *string) string {
	if v == nil {
		return "0"
	}
	return "1" + *v
}

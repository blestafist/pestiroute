package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const protectedConfigLimit = 1 << 20

type protectedConfig struct {
	Version    int                  `yaml:"version"`
	Server     protectedServer      `yaml:"server"`
	Storage    protectedStorage     `yaml:"storage"`
	Secrets    protectedSecrets     `yaml:"secrets"`
	Connectors []protectedConnector `yaml:"connectors"`
	Routes     []protectedRoute     `yaml:"routes"`
	Policies   map[string]string    `yaml:"policies"`
}

type protectedServer struct {
	Listen          string `yaml:"listen"`
	MaxRequestBytes int64  `yaml:"max_request_bytes"`
	ShutdownTimeout string `yaml:"shutdown_timeout"`
}

type protectedStorage struct {
	Driver string `yaml:"driver"`
	Path   string `yaml:"path"`
}

type protectedSecrets struct {
	MasterKeyFile string `yaml:"master_key_file"`
}

type protectedConnector struct {
	ID             string         `yaml:"id"`
	Kind           string         `yaml:"kind"`
	Implementation string         `yaml:"implementation"`
	Protocols      []string       `yaml:"protocols"`
	Settings       nativeSettings `yaml:"settings"`
}

// settings remain connector-owned; this shape is the selected native connector's schema.
type nativeSettings struct {
	BaseURL               string `yaml:"base_url"`
	UpstreamProtocol      string `yaml:"upstream_protocol"`
	Mode                  string `yaml:"mode"`
	CredentialEnv         string `yaml:"credential_env"`
	MaxRequestBodyBytes   int64  `yaml:"max_request_body_bytes"`
	MaxRequestHeaderBytes int64  `yaml:"max_request_header_bytes"`
	ConnectTimeout        string `yaml:"connect_timeout"`
	TLSHandshakeTimeout   string `yaml:"tls_handshake_timeout"`
	ResponseHeaderTimeout string `yaml:"response_header_timeout"`
	StreamIdleTimeout     string `yaml:"stream_idle_timeout"`
	Model                 string `yaml:"model"`
	AccountID             string `yaml:"account_id"`
	CredentialID          string `yaml:"credential_id"`
}

type protectedRoute struct {
	ID           string        `yaml:"id"`
	Protocol     string        `yaml:"protocol"`
	Mode         string        `yaml:"mode"`
	Model        string        `yaml:"model"`
	Adapter      string        `yaml:"adapter"`
	Policy       string        `yaml:"policy"`
	Budget       routeBudget   `yaml:"budget"`
	Requirements []string      `yaml:"requirements,omitempty"`
	Targets      []routeTarget `yaml:"targets"`
	Retry        *retryConfig  `yaml:"retry,omitempty"`
}

type routeBudget struct {
	UnknownEstimate    string `yaml:"unknown_estimate"`
	ConservativeTokens *int64 `yaml:"conservative_tokens,omitempty"`
}

type routeTarget struct {
	Connector string `yaml:"connector"`
	Account   string `yaml:"account"`
}

type retryConfig struct {
	MaxAttempts *int   `yaml:"max_attempts,omitempty"`
	Deadline    string `yaml:"deadline,omitempty"`
}

func loadProtectedYAML(path string) (protectedConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return protectedConfig{}, fmt.Errorf("config %q: %w", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, protectedConfigLimit+1))
	if err != nil {
		return protectedConfig{}, fmt.Errorf("config %q: %w", path, err)
	}
	if len(data) > protectedConfigLimit {
		return protectedConfig{}, fmt.Errorf("config %q: exceeds 1 MiB", path)
	}
	if !utf8.Valid(data) {
		return protectedConfig{}, fmt.Errorf("config %q: YAML must be UTF-8", path)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		return protectedConfig{}, fmt.Errorf("config %q: invalid YAML: %w", path, err)
	}
	if len(doc.Content) != 1 {
		return protectedConfig{}, fmt.Errorf("config %q: expected one YAML document", path)
	}
	if err := inspectYAMLNode(&doc); err != nil {
		return protectedConfig{}, fmt.Errorf("config %q: %w", path, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return protectedConfig{}, fmt.Errorf("config %q: expected one YAML document", path)
	}
	root := doc.Content[0]
	if err := validateConnectorSettingsFields(root); err != nil {
		return protectedConfig{}, fmt.Errorf("config %q: %w", path, err)
	}
	if err := validateYAMLTypes(root, reflect.TypeOf(protectedConfig{})); err != nil {
		return protectedConfig{}, fmt.Errorf("config %q: %w", path, err)
	}
	var c protectedConfig
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	if err := strict.Decode(&c); err != nil {
		return protectedConfig{}, fmt.Errorf("config %q: %w", path, err)
	}
	if err := validateProtectedConfig(c); err != nil {
		return protectedConfig{}, fmt.Errorf("config %q: %w", path, err)
	}
	return c, nil
}

func inspectYAMLNode(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return errors.New("YAML aliases and anchors are not allowed")
	}
	if node.Tag == "!!null" {
		return errors.New("explicit YAML null values are not allowed")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" {
				return errors.New("YAML mapping keys must be strings; merge keys are not allowed")
			}
			if _, ok := seen[key.Value]; ok {
				return fmt.Errorf("duplicate YAML key %q", key.Value)
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := inspectYAMLNode(child); err != nil {
			return err
		}
	}
	return nil
}

func validateConnectorSettingsFields(root *yaml.Node) error {
	var connectors *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "connectors" {
			connectors = root.Content[i+1]
			break
		}
	}
	if connectors == nil || connectors.Kind != yaml.SequenceNode {
		return nil // Shape validation below reports the structural error.
	}
	for i, connector := range connectors.Content {
		var implementation, settings *yaml.Node
		for j := 0; j+1 < len(connector.Content); j += 2 {
			switch connector.Content[j].Value {
			case "implementation":
				implementation = connector.Content[j+1]
			case "settings":
				settings = connector.Content[j+1]
			}
		}
		if implementation == nil || settings == nil || settings.Kind != yaml.MappingNode {
			continue // Required-field and type validation reports this case.
		}
		allowed := map[string]bool{}
		switch implementation.Value {
		case "pestiroute.responses.native":
			for _, key := range []string{"base_url", "upstream_protocol", "mode", "credential_env", "max_request_body_bytes", "max_request_header_bytes", "connect_timeout", "tls_handshake_timeout", "response_header_timeout", "stream_idle_timeout"} {
				allowed[key] = true
			}
		case "pestiroute.anthropic.messages":
			allowed["model"], allowed["account_id"], allowed["credential_id"] = true, true, true
		default:
			continue // Implementation validation reports this case.
		}
		for j := 0; j+1 < len(settings.Content); j += 2 {
			if !allowed[settings.Content[j].Value] {
				return fmt.Errorf("connectors[%d].settings has unsupported field %q", i, settings.Content[j].Value)
			}
		}
	}
	return nil
}

func validateYAMLTypes(node *yaml.Node, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("expected mapping, got %s", node.Tag)
		}
		fields := make(map[string]reflect.Type, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("yaml"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = field.Type
			}
		}
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			fieldType, ok := fields[key.Value]
			if ok {
				if err := validateYAMLTypes(value, fieldType); err != nil {
					return fmt.Errorf("%s: %w", key.Value, err)
				}
			}
		}
	case reflect.Slice:
		if node.Kind != yaml.SequenceNode {
			return fmt.Errorf("expected sequence, got %s", node.Tag)
		}
		for i, child := range node.Content {
			if err := validateYAMLTypes(child, typ.Elem()); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("expected mapping, got %s", node.Tag)
		}
		for i := 1; i < len(node.Content); i += 2 {
			if err := validateYAMLTypes(node.Content[i], typ.Elem()); err != nil {
				return fmt.Errorf("%s: %w", node.Content[i-1].Value, err)
			}
		}
	case reflect.String:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return fmt.Errorf("expected string, got %s", node.Tag)
		}
	case reflect.Int, reflect.Int64:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
			return fmt.Errorf("expected integer, got %s", node.Tag)
		}
	default:
		return fmt.Errorf("unsupported configuration type %s", typ)
	}
	return nil
}

func validateProtectedConfig(c protectedConfig) error {
	if c.Version != 1 {
		return errors.New("version must be integer 1")
	}
	if !requiredFieldsPresent(c) { // Root keys are checked separately by the node decoder below.
		return errors.New("incomplete protected configuration")
	}
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil || strings.TrimSpace(c.Server.Listen) == "" {
		return errors.New("server.listen must be a TCP address")
	}
	if c.Server.MaxRequestBytes <= 0 {
		return errors.New("server.max_request_bytes must be positive")
	}
	if err := positiveDuration("server.shutdown_timeout", c.Server.ShutdownTimeout); err != nil {
		return err
	}
	if c.Storage.Driver != "sqlite" || strings.TrimSpace(c.Storage.Path) == "" {
		return errors.New("storage requires driver sqlite and a non-empty path")
	}
	if strings.TrimSpace(c.Secrets.MasterKeyFile) == "" {
		return errors.New("secrets.master_key_file must be non-empty")
	}
	if len(c.Connectors) == 0 || len(c.Routes) == 0 || len(c.Policies) == 0 {
		return errors.New("connectors, routes, and policies must be non-empty")
	}
	connectors := make(map[string]struct{}, len(c.Connectors))
	for i, item := range c.Connectors {
		prefix := fmt.Sprintf("connectors[%d]", i)
		if strings.TrimSpace(item.ID) == "" || item.Kind != "connector" || len(item.Protocols) != 1 || item.Protocols[0] != responsesProtocol {
			return fmt.Errorf("%s has invalid identity, kind, implementation, or protocols", prefix)
		}
		if _, exists := connectors[item.ID]; exists {
			return fmt.Errorf("duplicate connector id %q", item.ID)
		}
		switch item.Implementation {
		case "pestiroute.responses.native":
			if item.Settings.Model != "" || item.Settings.AccountID != "" || item.Settings.CredentialID != "" {
				return fmt.Errorf("%s.settings has fields not supported by native connector", prefix)
			}
			if err := validateNativeSettings(prefix+".settings", item.Settings); err != nil {
				return err
			}
		case "pestiroute.anthropic.messages":
			if err := validateAnthropicSettings(prefix+".settings", item.Settings); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s has unsupported connector implementation %q", prefix, item.Implementation)
		}
		connectors[item.ID] = struct{}{}
	}
	for name, id := range c.Policies {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(id) == "" {
			return errors.New("policy names and SQLite policy IDs must be non-empty")
		}
	}
	seenRoutes := make(map[string]struct{}, len(c.Routes))
	type modelRouteGroup struct{ mode, routeID string }
	modelGroups := make(map[struct{ protocol, model string }]modelRouteGroup, len(c.Routes))
	for i := range c.Routes {
		route := &c.Routes[i]
		prefix := fmt.Sprintf("routes[%d]", i)
		if strings.TrimSpace(route.ID) == "" || route.Protocol != responsesProtocol || (route.Mode != "native" && route.Mode != "translation") || strings.TrimSpace(route.Model) == "" || route.Adapter != "pestiroute.responses.native" {
			return fmt.Errorf("%s has invalid identity, protocol, mode, model, or adapter", prefix)
		}
		if _, exists := seenRoutes[route.ID]; exists {
			return fmt.Errorf("duplicate route id %q", route.ID)
		}
		seenRoutes[route.ID] = struct{}{}
		modelKey := struct{ protocol, model string }{route.Protocol, route.Model}
		if previous, exists := modelGroups[modelKey]; exists && (previous.mode != route.Mode || previous.routeID != route.ID) {
			return fmt.Errorf("ambiguous protocol/model route group for %q and %q", route.Protocol, route.Model)
		}
		modelGroups[modelKey] = modelRouteGroup{mode: route.Mode, routeID: route.ID}
		if _, ok := c.Policies[route.Policy]; !ok {
			return fmt.Errorf("%s references unknown policy %q", prefix, route.Policy)
		}
		if err := validateBudget(prefix, route.Budget); err != nil {
			return err
		}
		if len(route.Targets) == 0 {
			return fmt.Errorf("%s.targets must be non-empty", prefix)
		}
		seenTargets := make(map[routeTarget]struct{}, len(route.Targets))
		seenAccounts := make(map[string]struct{}, len(route.Targets))
		seenConnectors := make(map[string]struct{}, len(route.Targets))
		translationTarget := false
		for j, target := range route.Targets {
			if _, ok := connectors[target.Connector]; !ok || strings.TrimSpace(target.Account) == "" {
				return fmt.Errorf("%s.targets[%d] has unknown connector or empty account", prefix, j)
			}
			if _, exists := seenTargets[target]; exists {
				return fmt.Errorf("%s.targets contains duplicate connector/account target", prefix)
			}
			if _, exists := seenAccounts[target.Account]; exists {
				return fmt.Errorf("%s.targets contains conflicting account target", prefix)
			}
			if _, exists := seenConnectors[target.Connector]; exists {
				return fmt.Errorf("%s.targets contains conflicting connector target", prefix)
			}
			seenTargets[target] = struct{}{}
			seenAccounts[target.Account] = struct{}{}
			seenConnectors[target.Connector] = struct{}{}
			for _, connector := range c.Connectors {
				if connector.ID != target.Connector {
					continue
				}
				if connector.Implementation == "pestiroute.anthropic.messages" {
					translationTarget = true
					if route.Mode != "translation" || connector.Settings.Model != route.Model || connector.Settings.AccountID != target.Account {
						return fmt.Errorf("%s target %q does not match Anthropic translation mode, model, and account settings", prefix, target.Connector)
					}
				} else if route.Mode == "translation" {
					return fmt.Errorf("%s target %q implementation does not match translation mode", prefix, target.Connector)
				}
			}
		}
		if translationTarget && route.Budget.UnknownEstimate == "reserve" && *route.Budget.ConservativeTokens < 4096 {
			return fmt.Errorf("%s.budget.conservative_tokens must be at least 4096 for Anthropic translation reserve", prefix)
		}
		if err := validateUniqueStrings(prefix+".requirements", route.Requirements); err != nil {
			return err
		}
		if route.Retry != nil {
			if route.Retry.MaxAttempts == nil {
				defaultAttempts := 1
				route.Retry.MaxAttempts = &defaultAttempts
			}
			attempts := *route.Retry.MaxAttempts
			if attempts < 1 || attempts > len(route.Targets) {
				return fmt.Errorf("%s.retry.max_attempts must be between 1 and target count", prefix)
			}
			if attempts > 1 && route.Retry.Deadline == "" {
				return fmt.Errorf("%s.retry.deadline is required when max_attempts > 1", prefix)
			}
			if route.Retry.Deadline != "" {
				if err := positiveDuration(prefix+".retry.deadline", route.Retry.Deadline); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateAnthropicSettings(prefix string, s nativeSettings) error {
	if strings.TrimSpace(s.Model) == "" || strings.TrimSpace(s.AccountID) == "" || strings.TrimSpace(s.CredentialID) == "" {
		return fmt.Errorf("%s requires non-empty model, account_id, and credential_id", prefix)
	}
	if s.BaseURL != "" || s.UpstreamProtocol != "" || s.Mode != "" || s.CredentialEnv != "" || s.MaxRequestBodyBytes != 0 || s.MaxRequestHeaderBytes != 0 || s.ConnectTimeout != "" || s.TLSHandshakeTimeout != "" || s.ResponseHeaderTimeout != "" || s.StreamIdleTimeout != "" {
		return fmt.Errorf("%s has fields not supported by Anthropic connector", prefix)
	}
	return nil
}

func requiredFieldsPresent(c protectedConfig) bool {
	return c.Server.Listen != "" && c.Server.MaxRequestBytes != 0 && c.Server.ShutdownTimeout != "" && c.Storage.Driver != "" && c.Storage.Path != "" && c.Secrets.MasterKeyFile != ""
}

func validateNativeSettings(prefix string, s nativeSettings) error {
	u, err := url.Parse(s.BaseURL)
	if err != nil || u == nil || u.Host == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname()))) {
		return fmt.Errorf("%s.base_url must be HTTPS or loopback HTTP URL", prefix)
	}
	if s.UpstreamProtocol != responsesProtocol || s.Mode != "native" || strings.TrimSpace(s.CredentialEnv) == "" || strings.Contains(s.CredentialEnv, "=") {
		return fmt.Errorf("%s.settings has invalid protocol, mode, or credential_env", prefix)
	}
	if value, ok := os.LookupEnv(s.CredentialEnv); !ok || value == "" {
		return fmt.Errorf("%s.settings credential_env is unset or empty", prefix)
	}
	if s.MaxRequestBodyBytes <= 0 || s.MaxRequestHeaderBytes <= 0 {
		return fmt.Errorf("%s.settings limits must be positive", prefix)
	}
	for _, item := range []struct{ name, value string }{{"connect_timeout", s.ConnectTimeout}, {"tls_handshake_timeout", s.TLSHandshakeTimeout}, {"response_header_timeout", s.ResponseHeaderTimeout}, {"stream_idle_timeout", s.StreamIdleTimeout}} {
		if err := positiveDuration(prefix+".settings."+item.name, item.value); err != nil {
			return err
		}
	}
	return nil
}

func validateBudget(prefix string, b routeBudget) error {
	switch b.UnknownEstimate {
	case "reject":
		if b.ConservativeTokens != nil {
			return fmt.Errorf("%s.budget forbids conservative_tokens with reject", prefix)
		}
	case "reserve":
		if b.ConservativeTokens == nil || *b.ConservativeTokens <= 0 {
			return fmt.Errorf("%s.budget requires positive conservative_tokens with reserve", prefix)
		}
	default:
		return fmt.Errorf("%s.budget.unknown_estimate must be reject or reserve", prefix)
	}
	return nil
}

func validateUniqueStrings(field string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s entries must be non-empty", field)
		}
		if _, ok := seen[value]; ok {
			return fmt.Errorf("%s contains duplicate %q", field, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func positiveDuration(field, value string) error {
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return fmt.Errorf("%s must be a positive Go duration", field)
	}
	return nil
}

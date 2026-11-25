package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// LLM providers the orchestrator supports.
const (
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
	ProviderGemini    = "gemini"
)

// SupportedProviders lists the accepted orchestrator.provider values.
var SupportedProviders = []string{ProviderOpenAI, ProviderAnthropic, ProviderGemini}

// SupportedIsolation lists the accepted worker isolation values.
var SupportedIsolation = []string{"host", "container", "vm"}

// Validate checks the controller config and returns every problem found, joined.
// An empty ListenAddr is allowed (it means DefaultListenAddr).
func (c ControllerConfig) Validate() error {
	var errs []error
	if c.ListenAddr != "" {
		if err := validateListenAddr(c.ListenAddr); err != nil {
			errs = append(errs, err)
		}
	}
	if strings.TrimSpace(c.DBPath) == "" {
		errs = append(errs, errors.New("db_path must not be empty"))
	}
	if c.PublicURL != "" {
		if err := validateURL("public_url", c.PublicURL, "http", "https"); err != nil {
			errs = append(errs, err)
		}
	}
	for _, o := range c.AllowedOrigins {
		if err := validateOriginPattern(o); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.Orchestrator.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := c.Forges.Validate(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Validate checks forge host overrides.
func (f ForgesConfig) Validate() error {
	var errs []error
	for host, kind := range f.Hosts {
		if strings.TrimSpace(host) == "" {
			errs = append(errs, errors.New("forges.hosts contains an empty host"))
			continue
		}
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "github", "gitlab", "forgejo":
		default:
			errs = append(errs, fmt.Errorf("forges.hosts[%q]: unsupported forge kind %q (want github, gitlab, or forgejo)", host, kind))
		}
	}
	return errors.Join(errs...)
}

// Validate checks the orchestrator LLM settings: a known provider and, if set, a
// well-formed http(s) base URL. An unknown provider is rejected rather than silently
// treated as OpenAI, which would send the configured key to api.openai.com.
func (o OrchestratorConfig) Validate() error {
	var errs []error
	if err := ValidateProvider(o.Provider); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(o.BaseURL) != "" {
		if err := validateURL("orchestrator.base_url", o.BaseURL, "http", "https"); err != nil {
			errs = append(errs, err)
		}
	}
	if o.MaxTokens < 0 {
		errs = append(errs, fmt.Errorf("orchestrator.max_tokens must be zero or positive, got %d", o.MaxTokens))
	}
	return errors.Join(errs...)
}

// ValidateProvider reports whether p names a supported LLM provider (case-insensitive;
// empty means openai). It mirrors orchestrator.ValidateProvider, which config cannot
// import (orchestrator imports config); a controller test keeps the lists in sync.
func ValidateProvider(p string) error {
	norm := strings.ToLower(strings.TrimSpace(p))
	if norm == "" {
		norm = ProviderOpenAI
	}
	for _, s := range SupportedProviders {
		if norm == s {
			return nil
		}
	}
	return fmt.Errorf("orchestrator.provider %q is not supported (want one of %s)", p, strings.Join(SupportedProviders, ", "))
}

// Validate checks the worker config. Callers (konsertd) should invoke it after loading.
func (w WorkerConfig) Validate() error {
	var errs []error
	if strings.TrimSpace(w.ControllerURL) != "" {
		if err := validateURL("controller_url", w.ControllerURL, "ws", "wss"); err != nil {
			errs = append(errs, err)
		}
	}
	iso := strings.ToLower(strings.TrimSpace(w.Isolation))
	found := false
	for _, s := range SupportedIsolation {
		if iso == s {
			found = true
			break
		}
	}
	if !found {
		errs = append(errs, fmt.Errorf("isolation %q is not supported (want one of %s)", w.Isolation, strings.Join(SupportedIsolation, ", ")))
	}
	if w.MaxConcurrent <= 0 {
		errs = append(errs, fmt.Errorf("max_concurrent must be positive, got %d", w.MaxConcurrent))
	}
	return errors.Join(errs...)
}

func validateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen_addr %q is invalid: %v", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("listen_addr %q has an invalid port", addr)
	}
	return nil
}

func validateURL(field, raw string, schemes ...string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%s %q is not a valid URL: %v", field, raw, err)
	}
	ok := false
	for _, s := range schemes {
		if strings.EqualFold(u.Scheme, s) {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("%s %q must use scheme %s", field, raw, strings.Join(schemes, " or "))
	}
	if u.Host == "" {
		return fmt.Errorf("%s %q has no host", field, raw)
	}
	return nil
}

func validateOriginPattern(o string) error {
	o = strings.TrimSpace(o)
	if o == "" {
		return errors.New("allowed_origins contains an empty entry")
	}
	pattern := o
	if strings.Contains(o, "://") {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			return fmt.Errorf("allowed_origins entry %q is not a valid origin", o)
		}
		pattern = u.Scheme + "://" + u.Host
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return fmt.Errorf("allowed_origins entry %q is not a valid pattern: %v", o, err)
	}
	return nil
}

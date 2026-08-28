package provider

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
)

// Identity is a conformance-tested Provider identity accepted by the first
// Gateway release. It is intentionally narrower than arbitrary upstream URLs.
type Identity string

type ExecutionSeam string

const (
	OpenAIResponsesSeam   ExecutionSeam = "openai-responses"
	OpenAICompatibleSeam  ExecutionSeam = "openai-compatible"
	AnthropicMessagesSeam ExecutionSeam = "anthropic-messages"
)

type IdentityProfile struct {
	CanonicalHost           string
	ResponseExecutionSeam   ExecutionSeam
	CapabilityExecutionSeam ExecutionSeam
	ModelDiscovery          bool
}

const (
	OpenAIIdentity    Identity = "openai"
	DeepSeekIdentity  Identity = "deepseek"
	AnthropicIdentity Identity = "anthropic"
	GeminiIdentity    Identity = "gemini"
)

var identityProfiles = map[Identity]IdentityProfile{
	OpenAIIdentity:    {CanonicalHost: "api.openai.com", ResponseExecutionSeam: OpenAIResponsesSeam, CapabilityExecutionSeam: OpenAICompatibleSeam, ModelDiscovery: true},
	DeepSeekIdentity:  {CanonicalHost: "api.deepseek.com", ResponseExecutionSeam: OpenAICompatibleSeam, CapabilityExecutionSeam: OpenAICompatibleSeam, ModelDiscovery: true},
	AnthropicIdentity: {CanonicalHost: "api.anthropic.com", ResponseExecutionSeam: AnthropicMessagesSeam, ModelDiscovery: true},
	GeminiIdentity:    {CanonicalHost: "generativelanguage.googleapis.com", ResponseExecutionSeam: OpenAICompatibleSeam, CapabilityExecutionSeam: OpenAICompatibleSeam, ModelDiscovery: true},
}

func ParseIdentity(value string) (Identity, error) {
	identity := Identity(value)
	if _, ok := identityProfiles[identity]; !ok {
		return "", fmt.Errorf("unsupported Provider identity %q", value)
	}
	return identity, nil
}

func (identity Identity) CanonicalHost() (string, bool) {
	profile, ok := identityProfiles[identity]
	return profile.CanonicalHost, ok
}

func (identity Identity) Profile() (IdentityProfile, bool) {
	profile, ok := identityProfiles[identity]
	return profile, ok
}

func (identity Identity) ValidateBaseURL(value string) error {
	profile, ok := identity.Profile()
	if !ok {
		return fmt.Errorf("unsupported Provider identity %q", identity)
	}
	baseURL, err := url.Parse(value)
	if err != nil || len(value) > 2048 || baseURL.Scheme != "https" || baseURL.Host == "" || baseURL.User != nil || baseURL.Fragment != "" || baseURL.RawQuery != "" {
		return errors.New("Provider Base URL must be absolute HTTPS without credentials, query, or fragments")
	}
	if baseURL.Hostname() != profile.CanonicalHost || baseURL.Port() != "" {
		return fmt.Errorf("Provider Base URL host must be %s", profile.CanonicalHost)
	}
	return nil
}

func SupportedIdentities() []Identity {
	identities := make([]Identity, 0, len(identityProfiles))
	for identity := range identityProfiles {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i] < identities[j] })
	return identities
}

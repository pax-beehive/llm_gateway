package provider_test

import (
	"reflect"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/provider"
)

func TestProviderIdentityRegistry(t *testing.T) {
	want := []provider.Identity{
		provider.AnthropicIdentity,
		provider.DeepSeekIdentity,
		provider.GeminiIdentity,
		provider.OpenAIIdentity,
	}
	if got := provider.SupportedIdentities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("supported identities = %v, want %v", got, want)
	}
	for _, identity := range want {
		parsed, err := provider.ParseIdentity(string(identity))
		if err != nil || parsed != identity {
			t.Fatalf("ParseIdentity(%q) = %q, %v", identity, parsed, err)
		}
		if host, ok := identity.CanonicalHost(); !ok || host == "" {
			t.Fatalf("CanonicalHost(%q) = %q, %v", identity, host, ok)
		}
		profile, ok := identity.Profile()
		if !ok || profile.ResponseExecutionSeam == "" || !profile.ModelDiscovery {
			t.Fatalf("Profile(%q) = %#v, %v", identity, profile, ok)
		}
	}
	if _, err := provider.ParseIdentity("custom"); err == nil {
		t.Fatal("expected custom Provider to be rejected")
	}
	if err := provider.OpenAIIdentity.ValidateBaseURL("https://attacker.example/v1"); err == nil {
		t.Fatal("expected non-canonical Provider host to be rejected")
	}
}

package providerconnection

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/secretcustody"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestProviderEndpointRegistryRejectsCredentialExfiltrationHosts(t *testing.T) {
	for _, test := range []struct {
		provider string
		url      string
		allowed  bool
	}{
		{"openai", "https://api.openai.com/v1", true},
		{"anthropic", "https://api.anthropic.com/v1", true},
		{"deepseek", "https://api.deepseek.com", true},
		{"gemini", "https://generativelanguage.googleapis.com/v1beta", true},
		{"openai", "https://api.openai.com.attacker.example/v1", false},
		{"openai", "https://attacker.example/v1", false},
		{"openai", "https://api.openai.com:444/v1", false},
	} {
		err := validateBaseURL(test.provider, test.url)
		if test.allowed && err != nil || !test.allowed && err == nil {
			t.Errorf("validateBaseURL(%q, %q) = %v", test.provider, test.url, err)
		}
	}
}

func TestRegisterIdempotencyHashDoesNotContainSecretVerifier(t *testing.T) {
	command := RegisterCommand{
		ID: "pc-test", Provider: "openai", DisplayName: "OpenAI", BaseURL: "https://api.openai.com/v1",
		Region: "us", CredentialScope: "models", CapabilityDeclaration: provider.CapabilityProfile{
			Revision: 1, Features: map[string]provider.CapabilitySupport{"text": provider.CapabilityNative},
		},
	}
	command.Secret = []byte("candidate-a")
	first, err := registerHash(command, "test")
	if err != nil {
		t.Fatal(err)
	}
	command.Secret = []byte("candidate-b")
	second, err := registerHash(command, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("PostgreSQL idempotency hash varies with Provider secret material")
	}
}

func TestLiveOperationsRequireServerPolicyAndZeroSpendBudget(t *testing.T) {
	actor := tenantadmin.ActorEnvelope{Type: "human", ID: "operator"}
	if _, err := (denyLiveOperationPolicy{}).Authorize(context.Background(), actor, OperationProbe); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("deny policy error = %v", err)
	}
	authorization, err := (StaticLiveOperationPolicy{
		Source: "deployment-change-123", ProbeMaxRequests: 1, DiscoveryMaxRequests: 20,
	}).Authorize(context.Background(), actor, OperationModelDiscovery)
	if err != nil || authorization.MaxProviderRequests != 20 || authorization.MaxSpendMicros != 0 {
		t.Fatalf("authorization = %#v err=%v", authorization, err)
	}
}

func TestSecretReferenceMustBeImmutable(t *testing.T) {
	if err := validateSecretReference(secretcustody.Reference{Name: "projects/p/secrets/s/versions/latest", Version: "latest"}); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("floating reference error = %v", err)
	}
	if err := validateSecretReference(secretcustody.Reference{Name: "projects/p/secrets/s/versions/7", Version: "7"}); err != nil {
		t.Fatalf("immutable reference error = %v", err)
	}
}

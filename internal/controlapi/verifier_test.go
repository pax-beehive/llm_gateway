package controlapi_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/controlapi"
)

func TestRS256VerifierValidatesHumanIAMAssertion(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	verifier, err := controlapi.NewRS256Verifier(controlapi.RS256VerifierConfig{
		PublicKey: &privateKey.PublicKey, Issuer: "https://iam.example.test", Audience: "llm-gateway-control-plane",
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	token := signedToken(t, privateKey, map[string]any{
		"iss": "https://iam.example.test", "aud": []string{"other", "llm-gateway-control-plane"},
		"sub": "human-123", "actor_type": "human", "acting_tenant_id": "tenant-a",
		"scope": "tenant:read tenant:write", "exp": now.Add(time.Minute).Unix(), "nbf": now.Add(-time.Minute).Unix(),
	})
	identity, err := verifier.Verify(context.Background(), "Bearer "+token)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ActorID != "human-123" || identity.ActorType != "human" || identity.ActingTenantID != "tenant-a" || strings.Join(identity.Scopes, ",") != "tenant:read,tenant:write" {
		t.Fatalf("identity = %#v", identity)
	}
	if _, err := verifier.Verify(context.Background(), "Bearer "+token+"tampered"); err == nil {
		t.Fatal("tampered assertion unexpectedly verified")
	}
	expired := signedToken(t, privateKey, map[string]any{
		"iss": "https://iam.example.test", "aud": "llm-gateway-control-plane", "sub": "human-123",
		"actor_type": "human", "scope": "tenant:read", "exp": now.Add(-time.Second).Unix(),
	})
	if _, err := verifier.Verify(context.Background(), "Bearer "+expired); err == nil {
		t.Fatal("expired assertion unexpectedly verified")
	}
}

func signedToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	return signedTokenWithKid(t, key, "", claims)
}

func signedTokenWithKid(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	headerValue := map[string]any{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		headerValue["kid"] = kid
	}
	header, err := json.Marshal(headerValue)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(encoded))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return encoded + "." + base64.RawURLEncoding.EncodeToString(signature)
}

package controlapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/controlapi"
)

func TestJWKSVerifierSelectsKidAndRefreshesForRotation(t *testing.T) {
	first, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	second, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var mutex sync.Mutex
	keys := map[string]*rsa.PublicKey{"key-1": &first.PublicKey}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		mutex.Lock()
		defer mutex.Unlock()
		requests++
		items := make([]map[string]string, 0, len(keys))
		for id, key := range keys {
			items = append(items, jwk(id, key))
		}
		payload, err := json.Marshal(map[string]any{"keys": items})
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(bytes.NewReader(payload)),
		}, nil
	})}
	now := time.Unix(1_800_000_000, 0)
	verifier, err := controlapi.NewJWKSVerifier(controlapi.JWKSVerifierConfig{
		URL: "https://iam.example.test/.well-known/jwks.json", Issuer: "https://iam.example.test", Audience: "control-plane",
		HTTPClient: client, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{
		"iss": "https://iam.example.test", "aud": "control-plane", "sub": "human-1",
		"actor_type": "human", "scope": "platform:tenants:read", "exp": now.Add(time.Minute).Unix(),
	}
	if _, err := verifier.Verify(context.Background(), "Bearer "+signedTokenWithKid(t, first, "key-1", claims)); err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), "Bearer "+signedTokenWithKid(t, second, "unknown", claims)); err == nil {
		t.Fatal("unknown kid unexpectedly verified")
	}
	if _, err := verifier.Verify(context.Background(), "Bearer "+signedTokenWithKid(t, second, "also-unknown", claims)); err == nil {
		t.Fatal("second unknown kid unexpectedly verified")
	}
	mutex.Lock()
	if requests != 1 {
		t.Fatalf("JWKS requests after unknown kid burst = %d, want 1", requests)
	}
	mutex.Unlock()
	mutex.Lock()
	keys = map[string]*rsa.PublicKey{"key-2": &second.PublicKey}
	mutex.Unlock()
	now = now.Add(31 * time.Second)
	identity, err := verifier.Verify(context.Background(), "Bearer "+signedTokenWithKid(t, second, "key-2", claims))
	if err != nil {
		t.Fatal(err)
	}
	if identity.ActorID != "human-1" {
		t.Fatalf("identity = %#v", identity)
	}
	mutex.Lock()
	if requests != 2 {
		t.Fatalf("JWKS requests after rotation = %d, want 2", requests)
	}
	mutex.Unlock()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func jwk(id string, key *rsa.PublicKey) map[string]string {
	exponent := big.NewInt(int64(key.E)).Bytes()
	return map[string]string{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": id,
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(exponent),
	}
}

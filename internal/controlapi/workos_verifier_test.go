package controlapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/controlapi"
)

func TestWorkOSVerifierMapsPermissionsAndPinsOrganization(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	document, _ := json.Marshal(map[string]any{"keys": []any{jwk("workos-key", &key.PublicKey)}})
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(document))}, nil
	})}
	now := time.Unix(1_800_000_000, 0)
	verifier, err := controlapi.NewWorkOSVerifier(controlapi.WorkOSVerifierConfig{
		JWKSURL: "https://api.workos.test/sso/jwks/client_1", Issuer: "https://api.workos.test/user_management/client_1",
		Audience: "client_1", AllowedOrganizationID: "org_operator", HTTPClient: client, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{
		"iss": "https://api.workos.test/user_management/client_1", "aud": "client_1", "sub": "user_1",
		"org_id": "org_operator", "permissions": []string{"platform:providers:read", "platform:providers:read", "platform:routing:write"},
		"exp": now.Add(time.Minute).Unix(),
	}
	identity, err := verifier.Verify(context.Background(), "Bearer "+signedTokenWithKid(t, key, "workos-key", claims))
	if err != nil {
		t.Fatal(err)
	}
	if identity.ActorType != "human" || identity.ActorID != "user_1" || identity.ActingTenantID != "" || !reflect.DeepEqual(identity.Scopes, []string{"platform:providers:read", "platform:routing:write"}) {
		t.Fatalf("identity = %#v", identity)
	}

	for name, mutate := range map[string]func(map[string]any){
		"other organization": func(value map[string]any) { value["org_id"] = "org_other" },
		"wrong audience":     func(value map[string]any) { value["aud"] = "client_other" },
		"wrong issuer":       func(value map[string]any) { value["iss"] = "https://evil.test" },
		"expired":            func(value map[string]any) { value["exp"] = now.Add(-time.Second).Unix() },
	} {
		t.Run(name, func(t *testing.T) {
			changed := make(map[string]any, len(claims))
			for key, value := range claims {
				changed[key] = value
			}
			mutate(changed)
			if _, err := verifier.Verify(context.Background(), "Bearer "+signedTokenWithKid(t, key, "workos-key", changed)); err == nil {
				t.Fatal("invalid WorkOS token unexpectedly verified")
			}
		})
	}
}

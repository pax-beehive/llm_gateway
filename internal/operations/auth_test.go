package operations_test

import (
	"context"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/operations"
)

func TestGatewayHMACBindsIdentityMethodPathBodyAndTime(t *testing.T) {
	now := time.Date(2026, 8, 29, 21, 0, 0, 0, time.UTC)
	key := []byte("0123456789abcdef0123456789abcdef")
	verifier, err := operations.NewHMACVerifier(map[string]string{"gateway-a": string(key)}, map[string]string{"gateway-a": "us-west"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"gateway_id":"gateway-a"}`)
	authorization, err := operations.GatewayAuthorization(key, "gateway-a", now, "POST", "/internal/v1/operations/gateway-observations", body)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := verifier.Verify(context.Background(), authorization, "POST", "/internal/v1/operations/gateway-observations", body)
	if err != nil || identity.GatewayID != "gateway-a" || identity.Region != "us-west" {
		t.Fatalf("identity/error = %#v / %v", identity, err)
	}
	for name, tampered := range map[string][3]string{
		"method": {"GET", "/internal/v1/operations/gateway-observations", string(body)},
		"path":   {"POST", "/internal/v1/operations/other", string(body)},
		"body":   {"POST", "/internal/v1/operations/gateway-observations", `{"gateway_id":"gateway-b"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(context.Background(), authorization, tampered[0], tampered[1], []byte(tampered[2])); err == nil {
				t.Fatal("tampered request authenticated")
			}
		})
	}

	stale, _ := operations.GatewayAuthorization(key, "gateway-a", now.Add(-3*time.Minute), "POST", "/internal/v1/operations/gateway-observations", body)
	if _, err := verifier.Verify(context.Background(), stale, "POST", "/internal/v1/operations/gateway-observations", body); err == nil {
		t.Fatal("stale assertion authenticated")
	}
}

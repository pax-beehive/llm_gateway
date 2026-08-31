package main

import (
	"context"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/operations"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestDevelopmentIdentityVerifierRequiresExplicitModeAndToken(t *testing.T) {
	t.Setenv("CONTROL_PLANE_DEV_MODE", "true")
	t.Setenv("CONTROL_PLANE_DEV_TOKEN", "local-admin-token")
	verifier, err := configureIdentityVerifier()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := verifier.Verify(context.Background(), "Bearer local-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	if identity.ActorType != "human" || identity.ActorID != "dev-operator" || len(identity.Scopes) != 2 || identity.Scopes[1] != tenantadmin.ScopePlatformWrite {
		t.Fatalf("identity = %#v", identity)
	}
	if _, err := verifier.Verify(context.Background(), "Bearer wrong"); err == nil {
		t.Fatal("wrong development token unexpectedly verified")
	}
}

func TestProductionIdentityVerifierFailsClosedWithoutIAMConfiguration(t *testing.T) {
	t.Setenv("CONTROL_PLANE_DEV_MODE", "false")
	t.Setenv("CONTROL_IAM_JWKS_URL", "")
	t.Setenv("CONTROL_IAM_ISSUER", "")
	t.Setenv("CONTROL_IAM_AUDIENCE", "")
	if _, err := configureIdentityVerifier(); err == nil {
		t.Fatal("production verifier unexpectedly accepted missing IAM configuration")
	}
}

func TestProductionIdentityVerifierSupportsExplicitDenyAllBootstrap(t *testing.T) {
	t.Setenv("CONTROL_PLANE_DEV_MODE", "false")
	t.Setenv("CONTROL_IAM_DENY_ALL", "true")
	verifier, err := configureIdentityVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), "Bearer anything"); err == nil {
		t.Fatal("deny-all production verifier accepted a human assertion")
	}
}

func TestCredentialPepperRingUsesExplicitCurrentVersion(t *testing.T) {
	t.Setenv("CONTROL_API_KEY_PEPPERS_JSON", `{"1":"old-control-pepper","2":"current-control-pepper"}`)
	t.Setenv("CONTROL_API_KEY_CURRENT_DIGEST_VERSION", "2")
	ring, err := credentialPepperRingFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if ring.CurrentVersion != 2 || string(ring.Peppers[1]) != "old-control-pepper" || string(ring.Peppers[2]) != "current-control-pepper" {
		t.Fatalf("pepper ring = %#v", ring)
	}
}

func TestControlPlaneReadinessDependencyStates(t *testing.T) {
	tests := map[string]struct {
		ready   error
		blocked error
	}{
		"schema":         {ready: controlSchemaReady(operations.CurrentDatabaseSchema), blocked: controlSchemaReady(operations.MinimumDatabaseSchema - 1)},
		"outbox":         {ready: controlOutboxReady(10, 10), blocked: controlOutboxReady(11, 10)},
		"secret custody": {ready: controlSecretCustodyReady(true), blocked: controlSecretCustodyReady(false)},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if test.ready != nil {
				t.Fatalf("ready state failed: %v", test.ready)
			}
			if test.blocked == nil {
				t.Fatal("unavailable state passed readiness")
			}
		})
	}
}

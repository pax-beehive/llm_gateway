package main

import (
	"context"
	"testing"

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
	t.Setenv("CONTROL_IAM_PUBLIC_KEY_FILE", "")
	t.Setenv("CONTROL_IAM_ISSUER", "")
	t.Setenv("CONTROL_IAM_AUDIENCE", "")
	if _, err := configureIdentityVerifier(); err == nil {
		t.Fatal("production verifier unexpectedly accepted missing IAM configuration")
	}
}

package main

import (
	"context"
	"testing"
)

func TestConfigureExportStoreKeepsFilesystemDevelopmentOnly(t *testing.T) {
	t.Setenv("METERING_EXPORT_BACKEND", "filesystem")
	t.Setenv("METERING_EXPORT_DIRECTORY", t.TempDir())
	if _, err := configureExportStore(context.Background(), true); err != nil {
		t.Fatalf("development filesystem store: %v", err)
	}
	if _, err := configureExportStore(context.Background(), false); err == nil {
		t.Fatal("production accepted filesystem Metering exports")
	}
}

func TestConfigureOperationsReporterRequiresProductionEndpointAndHTTPS(t *testing.T) {
	t.Setenv("METERING_OPERATIONS_URL", "")
	if _, err := configureOperationsReporter(false); err == nil {
		t.Fatal("production accepted missing Operations endpoint")
	}
	t.Setenv("METERING_OPERATIONS_URL", "http://control.example")
	if _, err := configureOperationsReporter(false); err == nil {
		t.Fatal("production accepted plaintext Operations endpoint")
	}
	t.Setenv("METERING_OPERATIONS_URL", "https://control.example")
	t.Setenv("METERING_CLOUD_RUN_AUDIENCE", "https://control.example")
	t.Setenv("METERING_OPERATIONS_HMAC_KEY", "metering-observation-hmac-key-00001")
	t.Setenv("METERING_ID", "metering-a")
	t.Setenv("METERING_REGION", "us-west1")
	if _, err := configureOperationsReporter(false); err != nil {
		t.Fatalf("production reporter: %v", err)
	}
}

func TestProductionVerifierSupportsExplicitDenyAllBootstrap(t *testing.T) {
	t.Setenv("METERING_IAM_DENY_ALL", "true")
	verifier, err := configureVerifier(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), "Bearer anything"); err == nil {
		t.Fatal("deny-all production verifier accepted a human assertion")
	}
}

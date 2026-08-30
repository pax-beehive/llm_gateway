package dbtransport_test

import (
	"testing"

	"github.com/toddzheng/llm-gateway/internal/dbtransport"
)

func TestRequireAuthenticatedEncryption(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		databaseURL string
		wantError   bool
	}{
		{name: "verify full", databaseURL: "postgres://db.example.test/gateway?sslmode=verify-full"},
		{name: "disable", databaseURL: "postgres://db.example.test/gateway?sslmode=disable", wantError: true},
		{name: "require without verification", databaseURL: "postgres://db.example.test/gateway?sslmode=require", wantError: true},
		{name: "prefer has plaintext fallback", databaseURL: "postgres://db.example.test/gateway?sslmode=prefer", wantError: true},
		{name: "invalid", databaseURL: "://", wantError: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := dbtransport.RequireAuthenticatedEncryption(test.databaseURL)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError=%t", err, test.wantError)
			}
		})
	}
}

func TestRequireAuthenticatedTransport(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		databaseURL string
		instance    string
		wantError   bool
	}{
		{name: "verified direct TLS", databaseURL: "postgres://db.example.test/gateway?sslmode=verify-full"},
		{name: "Cloud SQL Connector", databaseURL: "user=gateway dbname=gateway", instance: "pax-fde-prod:us-west1:llm-gateway-prod-postgres"},
		{name: "direct plaintext", databaseURL: "postgres://db.example.test/gateway?sslmode=disable", wantError: true},
		{name: "malformed connector instance", databaseURL: "user=gateway dbname=gateway", instance: "pax-fde-prod/us-west1/instance", wantError: true},
		{name: "uppercase connector instance", databaseURL: "user=gateway dbname=gateway", instance: "PAX:us-west1:instance", wantError: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := dbtransport.RequireAuthenticatedTransport(test.databaseURL, test.instance)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError=%t", err, test.wantError)
			}
		})
	}
}

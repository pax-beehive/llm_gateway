package operations_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/operations"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestMeteringReporterSignsContentFreeObservation(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	key := []byte("metering-observation-hmac-key-00001")
	verifier, _ := operations.NewMeteringHMACVerifier(map[string]string{"metering-a": string(key)}, map[string]string{"metering-a": "us-west1"}, func() time.Time { return now })
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		identity, err := verifier.Verify(context.Background(), request.Header.Get("Authorization"), request.Method, request.URL.RequestURI(), body)
		if err != nil || identity.MeteringID != "metering-a" || strings.Contains(string(body), "prompt") {
			t.Fatalf("identity/body/error = %#v/%s/%v", identity, body, err)
		}
		return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(`{"status":"accepted"}`)), Header: make(http.Header)}, nil
	})}
	reporter, err := operations.NewMeteringReporter("https://control.example/base", "metering-a", "us-west1", key, client, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.Report(context.Background(), operations.MeteringObservation{ProjectionGeneration: 2, ProjectionCutoff: now.Add(-time.Second), StartedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func TestReporterSignsTheExactObservationRequest(t *testing.T) {
	now := time.Date(2026, 8, 29, 23, 0, 0, 0, time.UTC)
	key := []byte("0123456789abcdef0123456789abcdef")
	verifier, _ := operations.NewHMACVerifier(map[string]string{"gateway-a": string(key)}, map[string]string{"gateway-a": "us-west"}, func() time.Time { return now })
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := verifier.Verify(context.Background(), request.Header.Get("Authorization"), request.Method, request.URL.RequestURI(), body)
		if err != nil || identity.GatewayID != "gateway-a" {
			t.Fatalf("identity/error = %#v/%v", identity, err)
		}
		if strings.Contains(string(body), "secret") || strings.Contains(string(body), "prompt") {
			t.Fatalf("content leaked: %s", body)
		}
		return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(`{"status":"accepted"}`)), Header: make(http.Header)}, nil
	})}
	reporter, err := operations.NewReporter("https://control.example/base", "gateway-a", "us-west", key, client, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.Report(context.Background(), operations.GatewayObservation{BuildSHA: "abc1234", DatabaseSchemaVersion: 21, StartedAt: now.Add(-time.Hour), ObservedAt: now, Consumers: []operations.ConsumerObservation{}}); err != nil {
		t.Fatal(err)
	}
}

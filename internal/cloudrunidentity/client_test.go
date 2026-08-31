package cloudrunidentity

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"cloud.google.com/go/auth"
)

type tokenProviderFunc func(context.Context) (*auth.Token, error)

func (function tokenProviderFunc) Token(ctx context.Context) (*auth.Token, error) {
	return function(ctx)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestTransportAddsPlatformIdentityAndPreservesApplicationAuthorization(t *testing.T) {
	original, err := http.NewRequest(http.MethodGet, "https://control.example/internal", nil)
	if err != nil {
		t.Fatal(err)
	}
	original.Header.Set("Authorization", "Gateway-HMAC application-signature")
	var observed *http.Request
	transport := &transport{
		tokens: tokenProviderFunc(func(context.Context) (*auth.Token, error) {
			return &auth.Token{Value: "cloud-run-id-token"}, nil
		}),
		base: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			observed = request
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: request}, nil
		}),
	}
	if _, err := transport.RoundTrip(original); err != nil {
		t.Fatal(err)
	}
	if got := observed.Header.Get("Authorization"); got != "Gateway-HMAC application-signature" {
		t.Fatalf("application Authorization = %q", got)
	}
	if got := observed.Header.Get(authorizationHeader); got != "Bearer cloud-run-id-token" {
		t.Fatalf("Cloud Run Authorization = %q", got)
	}
	if got := original.Header.Get(authorizationHeader); got != "" {
		t.Fatalf("original request was mutated: %q", got)
	}
}

func TestTransportPropagatesTokenFailureWithoutSendingRequest(t *testing.T) {
	want := errors.New("metadata unavailable")
	called := false
	transport := &transport{
		tokens: tokenProviderFunc(func(context.Context) (*auth.Token, error) { return nil, want }),
		base: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			called = true
			return nil, nil
		}),
	}
	request, _ := http.NewRequest(http.MethodGet, "https://control.example/internal", nil)
	if _, err := transport.RoundTrip(request); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("request was sent after token failure")
	}
}

func TestTransportRejectsEmptyToken(t *testing.T) {
	transport := &transport{tokens: tokenProviderFunc(func(context.Context) (*auth.Token, error) {
		return &auth.Token{}, nil
	})}
	request, _ := http.NewRequest(http.MethodGet, "https://control.example/internal", nil)
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("expected empty-token error")
	}
}

package secretcustody_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/secretcustody"
)

func TestMemoryPutIsIdempotentAndRejectsChangedMaterial(t *testing.T) {
	store := secretcustody.NewMemory()
	first, err := store.Put(context.Background(), "stable-key", []byte("secret-a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(context.Background(), "stable-key", []byte("secret-a"))
	if err != nil || first != second {
		t.Fatalf("idempotent reference = %#v/%#v err=%v", first, second, err)
	}
	if _, err := store.Put(context.Background(), "stable-key", []byte("secret-b")); err != secretcustody.ErrConflict {
		t.Fatalf("changed material error = %v", err)
	}
}

func TestGCPSecretManagerStoresAndAccessesVersionWithoutLeakingErrorBodies(t *testing.T) {
	material := []byte("provider-secret")
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("Authorization") != "Bearer workload-token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch calls {
		case 1:
			if request.Method != http.MethodPost || !strings.Contains(request.URL.Path, "/projects/project-a/secrets") || request.URL.Query().Get("secretId") == "" {
				t.Fatalf("create request = %s %s", request.Method, request.URL.String())
			}
			return response(request, http.StatusOK, `{}`), nil
		case 2:
			if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, ":addVersion") {
				t.Fatalf("version request = %s %s", request.Method, request.URL.String())
			}
			body, _ := io.ReadAll(request.Body)
			if !strings.Contains(string(body), base64.StdEncoding.EncodeToString(material)) {
				t.Fatalf("version payload = %s", body)
			}
			secretName := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/v1/"), ":addVersion")
			return response(request, http.StatusOK, `{"name":"`+secretName+`/versions/7"}`), nil
		case 3:
			if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/versions/7:access") {
				t.Fatalf("access request = %s %s", request.Method, request.URL.String())
			}
			secretName := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/v1/"), "/versions/7:access")
			return response(request, http.StatusOK, `{"name":"`+secretName+`/versions/7","payload":{"data":"`+base64.StdEncoding.EncodeToString(material)+`"}}`), nil
		default:
			t.Fatalf("unexpected request %s", request.URL.String())
			return nil, nil
		}
	})}
	store, err := secretcustody.NewGCP(secretcustody.GCPConfig{
		ProjectID: "project-a", Endpoint: "https://secretmanager.test/v1", HTTPClient: client,
		TokenProvider: staticTokenProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := store.Put(context.Background(), "stable-key", material)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Access(context.Background(), reference)
	if err != nil || string(got) != string(material) {
		t.Fatalf("accessed material = %q err=%v", got, err)
	}
}

func TestGCPSecretManagerIdempotentPutReturnsExactVersion(t *testing.T) {
	material := []byte("provider-secret")
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(request, http.StatusConflict, `{}`), nil
		}
		secretName := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/v1/"), "/versions/latest:access")
		return response(request, http.StatusOK, `{"name":"`+secretName+`/versions/11","payload":{"data":"`+base64.StdEncoding.EncodeToString(material)+`"}}`), nil
	})}
	store, err := secretcustody.NewGCP(secretcustody.GCPConfig{
		ProjectID: "project-a", Endpoint: "https://secretmanager.test/v1", HTTPClient: client,
		TokenProvider: staticTokenProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := store.Put(context.Background(), "stable-key", material)
	if err != nil {
		t.Fatal(err)
	}
	if reference.Version != "11" || strings.HasSuffix(reference.Name, "/latest") {
		t.Fatalf("reference = %#v", reference)
	}
}

func TestGCPSecretManagerConflictFailsClosedWhenExistingVersionCannotBeRead(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(request, http.StatusConflict, `{}`), nil
		}
		if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/versions/latest:access") {
			t.Fatalf("unexpected request = %s %s", request.Method, request.URL.String())
		}
		return response(request, http.StatusServiceUnavailable, `{"error":{"message":"transient"}}`), nil
	})}
	store, err := secretcustody.NewGCP(secretcustody.GCPConfig{
		ProjectID: "project-a", Endpoint: "https://secretmanager.test/v1", HTTPClient: client,
		TokenProvider: staticTokenProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), "stable-key", []byte("provider-secret")); err == nil {
		t.Fatal("Put succeeded after an ambiguous existing-version read")
	}
	if calls != 2 {
		t.Fatalf("calls = %d; an addVersion request must not follow ambiguous access", calls)
	}
}

type staticTokenProvider struct{}

func (staticTokenProvider) Token(context.Context) (secretcustody.Token, error) {
	return secretcustody.Token{AccessToken: "workload-token"}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func response(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}
}

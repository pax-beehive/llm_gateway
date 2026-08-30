package metering_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/metering"
)

func TestGCSExportStoreVerifiesRegionAndUsesConditionalCreate(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: meteringRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("Authorization") != "Bearer workload-token" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		switch calls {
		case 1:
			if request.Method != http.MethodGet || !strings.Contains(request.URL.Path, "/storage/v1/b/metering-bucket") {
				t.Fatalf("metadata request = %s %s", request.Method, request.URL.String())
			}
			return meteringResponse(request, http.StatusOK, `{"name":"metering-bucket","location":"US-WEST1","locationType":"region"}`), nil
		case 2:
			if request.Method != http.MethodPost || request.URL.Query().Get("uploadType") != "media" || request.URL.Query().Get("ifGenerationMatch") != "0" || request.URL.Query().Get("name") != "exports/export.csv" {
				t.Fatalf("upload request = %s %s", request.Method, request.URL.String())
			}
			body, _ := io.ReadAll(request.Body)
			if string(body) != "stable" {
				t.Fatalf("upload body = %q", body)
			}
			return meteringResponse(request, http.StatusOK, `{}`), nil
		case 3:
			if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/metering-bucket/exports/export.csv") {
				t.Fatalf("download request = %s %s", request.Method, request.URL.String())
			}
			return meteringResponse(request, http.StatusOK, "stable"), nil
		default:
			t.Fatalf("unexpected request: %s", request.URL.String())
			return nil, nil
		}
	})}
	store, err := metering.NewGCSExportStore(context.Background(), metering.GCSExportStoreConfig{
		Bucket: "metering-bucket", Prefix: "exports", Region: "us-west1", Endpoint: "https://storage.test",
		HTTPClient: client, AccessToken: func(context.Context) (string, error) { return "workload-token", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "export.csv", []byte("stable")); err != nil {
		t.Fatal(err)
	}
	payload, err := store.Get(context.Background(), "export.csv")
	if err != nil || string(payload) != "stable" {
		t.Fatalf("payload/error = %q/%v", payload, err)
	}
}

func TestGCSExportStoreRetryReadsAndComparesExistingObject(t *testing.T) {
	for _, test := range []struct {
		name, existing string
		wantError      bool
	}{{"same", "stable", false}, {"different", "changed", true}} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: meteringRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return meteringResponse(request, http.StatusOK, `{"name":"metering-bucket","location":"US-WEST1","locationType":"region"}`), nil
				}
				if calls == 2 {
					return meteringResponse(request, http.StatusPreconditionFailed, `{}`), nil
				}
				return meteringResponse(request, http.StatusOK, test.existing), nil
			})}
			store, err := metering.NewGCSExportStore(context.Background(), metering.GCSExportStoreConfig{
				Bucket: "metering-bucket", Region: "us-west1", Endpoint: "https://storage.test", HTTPClient: client,
				AccessToken: func(context.Context) (string, error) { return "workload-token", nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			err = store.Put(context.Background(), "export.csv", []byte("stable"))
			if (err != nil) != test.wantError {
				t.Fatalf("Put error = %v", err)
			}
		})
	}
}

func TestGCSExportStoreRejectsMultiRegionBucket(t *testing.T) {
	client := &http.Client{Transport: meteringRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return meteringResponse(request, http.StatusOK, `{"name":"metering-bucket","location":"US","locationType":"multi-region"}`), nil
	})}
	_, err := metering.NewGCSExportStore(context.Background(), metering.GCSExportStoreConfig{
		Bucket: "metering-bucket", Region: "us-west1", Endpoint: "https://storage.test", HTTPClient: client,
		AccessToken: func(context.Context) (string, error) { return "workload-token", nil },
	})
	if err == nil {
		t.Fatal("multi-region bucket was accepted")
	}
}

type meteringRoundTripFunc func(*http.Request) (*http.Response, error)

func (function meteringRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func meteringResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}
}

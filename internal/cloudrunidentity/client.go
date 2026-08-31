// Package cloudrunidentity authenticates private service-to-service Cloud Run
// requests without replacing the application's own Authorization header.
package cloudrunidentity

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials/idtoken"
)

const authorizationHeader = "X-Serverless-Authorization"

// NewClient returns an HTTP client that attaches an ADC-backed Cloud Run ID
// token whose audience is the receiving service URL.
func NewClient(audience string) (*http.Client, error) {
	audience = strings.TrimSpace(audience)
	if audience == "" {
		return nil, errors.New("Cloud Run identity audience is required")
	}
	return &http.Client{Transport: &transport{tokens: &lazyTokenProvider{audience: audience}}}, nil
}

type lazyTokenProvider struct {
	audience    string
	once        sync.Once
	credentials auth.TokenProvider
	err         error
}

func (provider *lazyTokenProvider) Token(ctx context.Context) (*auth.Token, error) {
	provider.once.Do(func() {
		provider.credentials, provider.err = idtoken.NewCredentials(&idtoken.Options{Audience: provider.audience})
	})
	if provider.err != nil {
		return nil, provider.err
	}
	return provider.credentials.Token(ctx)
}

type transport struct {
	tokens auth.TokenProvider
	base   http.RoundTripper
}

func (value *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("Cloud Run identity request is required")
	}
	token, err := value.tokens.Token(request.Context())
	if err != nil {
		return nil, err
	}
	if token == nil || strings.TrimSpace(token.Value) == "" {
		return nil, errors.New("Cloud Run identity provider returned an empty token")
	}
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set(authorizationHeader, "Bearer "+token.Value)
	base := value.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

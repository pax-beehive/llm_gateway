package controlapi

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type JWKSVerifierConfig struct {
	URL                string
	Issuer             string
	Audience           string
	HTTPClient         *http.Client
	Now                func() time.Time
	CacheTTL           time.Duration
	MinRefreshInterval time.Duration
	RefreshTimeout     time.Duration
	ClockSkew          time.Duration
	AllowInsecureHTTP  bool
}

type JWKSVerifier struct {
	url                string
	issuer             string
	audience           string
	client             *http.Client
	now                func() time.Time
	cacheTTL           time.Duration
	minRefreshInterval time.Duration
	refreshTimeout     time.Duration
	clockSkew          time.Duration
	mutex              sync.Mutex
	keys               map[string]*rsa.PublicKey
	validUntil         time.Time
	lastRefreshAttempt time.Time
	refreshing         chan struct{}
}

func NewJWKSVerifier(config JWKSVerifierConfig) (*JWKSVerifier, error) {
	parsed, err := url.Parse(config.URL)
	if err != nil || parsed.Host == "" || parsed.Scheme == "" {
		return nil, errors.New("JWKS verifier requires an absolute URL")
	}
	if parsed.Scheme != "https" && !config.AllowInsecureHTTP {
		return nil, errors.New("JWKS URL must use HTTPS")
	}
	if strings.TrimSpace(config.Issuer) == "" || strings.TrimSpace(config.Audience) == "" {
		return nil, errors.New("JWKS verifier requires issuer and audience")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.CacheTTL == 0 {
		config.CacheTTL = 5 * time.Minute
	}
	if config.CacheTTL < time.Minute || config.CacheTTL > 24*time.Hour {
		return nil, errors.New("JWKS cache TTL must be between one minute and 24 hours")
	}
	if config.MinRefreshInterval == 0 {
		config.MinRefreshInterval = 30 * time.Second
	}
	if config.MinRefreshInterval < time.Second || config.MinRefreshInterval > 5*time.Minute {
		return nil, errors.New("JWKS minimum refresh interval must be between one second and five minutes")
	}
	if config.RefreshTimeout == 0 {
		config.RefreshTimeout = 5 * time.Second
	}
	if config.RefreshTimeout < 250*time.Millisecond || config.RefreshTimeout > 30*time.Second {
		return nil, errors.New("JWKS refresh timeout must be between 250 milliseconds and 30 seconds")
	}
	if config.ClockSkew < 0 || config.ClockSkew > 5*time.Minute {
		return nil, errors.New("JWKS verifier clock skew must be between zero and five minutes")
	}
	return &JWKSVerifier{
		url: config.URL, issuer: config.Issuer, audience: config.Audience,
		client: config.HTTPClient, now: config.Now, cacheTTL: config.CacheTTL,
		minRefreshInterval: config.MinRefreshInterval, refreshTimeout: config.RefreshTimeout,
		clockSkew: config.ClockSkew,
		keys:      make(map[string]*rsa.PublicKey),
	}, nil
}

func (verifier *JWKSVerifier) Verify(ctx context.Context, authorization string) (VerifiedIdentity, error) {
	kid, err := assertionKeyID(authorization)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	key, err := verifier.key(ctx, kid)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	static, err := NewRS256Verifier(RS256VerifierConfig{
		PublicKey: key, Issuer: verifier.issuer, Audience: verifier.audience,
		Now: verifier.now, ClockSkew: verifier.clockSkew,
	})
	if err != nil {
		return VerifiedIdentity{}, err
	}
	return static.Verify(ctx, authorization)
}

func (verifier *JWKSVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	for {
		now := verifier.now()
		verifier.mutex.Lock()
		key := verifier.keys[kid]
		cacheValid := now.Before(verifier.validUntil)
		if key != nil && cacheValid {
			verifier.mutex.Unlock()
			return key, nil
		}
		if verifier.refreshing != nil {
			refreshing := verifier.refreshing
			verifier.mutex.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-refreshing:
				continue
			}
		}
		if !verifier.lastRefreshAttempt.IsZero() && now.Sub(verifier.lastRefreshAttempt) < verifier.minRefreshInterval {
			verifier.mutex.Unlock()
			if cacheValid {
				return nil, errors.New("identity assertion signing key is unknown")
			}
			return nil, errors.New("Human IAM signing keys are temporarily unavailable")
		}
		refreshing := make(chan struct{})
		verifier.refreshing = refreshing
		verifier.lastRefreshAttempt = now
		verifier.mutex.Unlock()

		refreshContext, cancelRefresh := context.WithTimeout(context.Background(), verifier.refreshTimeout)
		keys, err := verifier.loadKeys(refreshContext)
		cancelRefresh()
		verifier.mutex.Lock()
		if err == nil {
			verifier.keys = keys
			verifier.validUntil = now.Add(verifier.cacheTTL)
		}
		close(refreshing)
		verifier.refreshing = nil
		verifier.mutex.Unlock()
		if err != nil {
			return nil, err
		}
	}
}

func (verifier *JWKSVerifier) loadKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, verifier.url, nil)
	if err != nil {
		return nil, errors.New("load Human IAM signing keys")
	}
	request.Header.Set("Accept", "application/json")
	response, err := verifier.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("load Human IAM signing keys: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return nil, fmt.Errorf("load Human IAM signing keys: HTTP %d", response.StatusCode)
	}
	var document struct {
		Keys []jsonWebKey `json:"keys"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("load Human IAM signing keys: invalid JWKS")
	}
	keys := make(map[string]*rsa.PublicKey, len(document.Keys))
	for _, candidate := range document.Keys {
		if candidate.KeyType != "RSA" || candidate.KeyID == "" || candidate.Algorithm != "RS256" || candidate.Use != "sig" {
			continue
		}
		key, err := candidate.publicKey()
		if err != nil {
			return nil, fmt.Errorf("load Human IAM signing key %q: %w", candidate.KeyID, err)
		}
		if _, duplicate := keys[candidate.KeyID]; duplicate {
			return nil, errors.New("load Human IAM signing keys: duplicate kid")
		}
		keys[candidate.KeyID] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("load Human IAM signing keys: no usable RS256 keys")
	}
	return keys, nil
}

type jsonWebKey struct {
	KeyType   string `json:"kty"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Modulus   string `json:"n"`
	Exponent  string `json:"e"`
}

func (key jsonWebKey) publicKey() (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(key.Modulus)
	if err != nil || len(modulus) < 256 {
		return nil, errors.New("invalid RSA modulus")
	}
	exponentBytes, err := base64.RawURLEncoding.DecodeString(key.Exponent)
	if err != nil || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
		return nil, errors.New("invalid RSA exponent")
	}
	exponent := new(big.Int).SetBytes(exponentBytes)
	if !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > int64(^uint(0)>>1) {
		return nil, errors.New("invalid RSA exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(exponent.Int64())}, nil
}

func assertionKeyID(authorization string) (string, error) {
	token, err := bearerAssertion(authorization)
	if err != nil {
		return "", err
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("identity assertion is not a compact JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", errors.New("identity assertion header is invalid")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(payload, &header); err != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return "", errors.New("identity assertion requires RS256 and kid")
	}
	return header.KeyID, nil
}

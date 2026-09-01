package controlapi

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// WorkOSVerifier validates AuthKit access tokens and maps their permissions to
// the control-plane scope vocabulary. Organization membership is pinned to one
// explicitly configured operator organization.
type WorkOSVerifier struct {
	keys         *JWKSVerifier
	issuer       string
	audience     string
	organization string
	now          func() time.Time
	clockSkew    time.Duration
}

type WorkOSVerifierConfig struct {
	JWKSURL               string
	Issuer                string
	Audience              string
	AllowedOrganizationID string
	HTTPClient            *http.Client
	Now                   func() time.Time
	ClockSkew             time.Duration
}

func NewWorkOSVerifier(config WorkOSVerifierConfig) (*WorkOSVerifier, error) {
	if strings.TrimSpace(config.AllowedOrganizationID) == "" {
		return nil, errors.New("WorkOS verifier requires an allowed organization ID")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	keys, err := NewJWKSVerifier(JWKSVerifierConfig{
		URL: config.JWKSURL, Issuer: config.Issuer, Audience: config.Audience,
		HTTPClient: config.HTTPClient, Now: config.Now, ClockSkew: config.ClockSkew,
	})
	if err != nil {
		return nil, err
	}
	return &WorkOSVerifier{
		keys: keys, issuer: config.Issuer, audience: config.Audience,
		organization: config.AllowedOrganizationID, now: config.Now, clockSkew: config.ClockSkew,
	}, nil
}

func (verifier *WorkOSVerifier) Verify(ctx context.Context, authorization string) (VerifiedIdentity, error) {
	token, err := bearerAssertion(authorization)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	if len(token) > 16<<10 {
		return VerifiedIdentity{}, errors.New("identity assertion is too large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return VerifiedIdentity{}, errors.New("identity assertion is not a compact JWT")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return VerifiedIdentity{}, errors.New("identity assertion header is invalid")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return VerifiedIdentity{}, errors.New("identity assertion must use RS256 with a key ID")
	}
	key, err := verifier.keys.key(ctx, header.KeyID)
	if err != nil {
		return VerifiedIdentity{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return VerifiedIdentity{}, errors.New("identity assertion signature is invalid")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return VerifiedIdentity{}, errors.New("identity assertion signature is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return VerifiedIdentity{}, errors.New("identity assertion payload is invalid")
	}
	var claims struct {
		Issuer       string    `json:"iss"`
		Audience     audiences `json:"aud"`
		Subject      string    `json:"sub"`
		Organization string    `json:"org_id"`
		Permissions  []string  `json:"permissions"`
		ExpiresAt    int64     `json:"exp"`
		NotBefore    int64     `json:"nbf"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return VerifiedIdentity{}, errors.New("identity assertion claims are invalid")
	}
	now := verifier.now().UTC()
	if claims.Issuer != verifier.issuer || !claims.Audience.Contains(verifier.audience) {
		return VerifiedIdentity{}, errors.New("identity assertion issuer or audience is invalid")
	}
	if claims.Subject == "" || claims.Organization != verifier.organization || claims.ExpiresAt == 0 {
		return VerifiedIdentity{}, errors.New("identity assertion required claims are missing or invalid")
	}
	if !now.Before(time.Unix(claims.ExpiresAt, 0).Add(verifier.clockSkew)) {
		return VerifiedIdentity{}, errors.New("identity assertion has expired")
	}
	if claims.NotBefore != 0 && now.Add(verifier.clockSkew).Before(time.Unix(claims.NotBefore, 0)) {
		return VerifiedIdentity{}, errors.New("identity assertion is not active")
	}
	return VerifiedIdentity{ActorType: "human", ActorID: claims.Subject, Scopes: uniqueScopes(claims.Permissions)}, nil
}

package controlapi

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

type RS256VerifierConfig struct {
	PublicKey *rsa.PublicKey
	Issuer    string
	Audience  string
	Now       func() time.Time
	ClockSkew time.Duration
}

type RS256Verifier struct {
	publicKey *rsa.PublicKey
	issuer    string
	audience  string
	now       func() time.Time
	clockSkew time.Duration
}

func NewRS256Verifier(config RS256VerifierConfig) (*RS256Verifier, error) {
	if config.PublicKey == nil || strings.TrimSpace(config.Issuer) == "" || strings.TrimSpace(config.Audience) == "" {
		return nil, errors.New("RS256 verifier requires a public key, issuer, and audience")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.ClockSkew < 0 || config.ClockSkew > 5*time.Minute {
		return nil, errors.New("RS256 verifier clock skew must be between zero and five minutes")
	}
	return &RS256Verifier{
		publicKey: config.PublicKey, issuer: config.Issuer, audience: config.Audience,
		now: config.Now, clockSkew: config.ClockSkew,
	}, nil
}

func (verifier *RS256Verifier) Verify(_ context.Context, authorization string) (VerifiedIdentity, error) {
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
	headerPayload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return VerifiedIdentity{}, errors.New("identity assertion header is invalid")
	}
	var header struct {
		Algorithm string `json:"alg"`
	}
	if err := json.Unmarshal(headerPayload, &header); err != nil || header.Algorithm != "RS256" {
		return VerifiedIdentity{}, errors.New("identity assertion must use RS256")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return VerifiedIdentity{}, errors.New("identity assertion signature is invalid")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(verifier.publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return VerifiedIdentity{}, errors.New("identity assertion signature is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return VerifiedIdentity{}, errors.New("identity assertion payload is invalid")
	}
	var claims assertionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return VerifiedIdentity{}, errors.New("identity assertion claims are invalid")
	}
	now := verifier.now().UTC()
	if claims.Issuer != verifier.issuer || !claims.Audience.Contains(verifier.audience) {
		return VerifiedIdentity{}, errors.New("identity assertion issuer or audience is invalid")
	}
	if claims.Subject == "" || claims.ActorType == "" || claims.ExpiresAt == 0 {
		return VerifiedIdentity{}, errors.New("identity assertion required claims are missing")
	}
	if !now.Before(time.Unix(claims.ExpiresAt, 0).Add(verifier.clockSkew)) {
		return VerifiedIdentity{}, errors.New("identity assertion has expired")
	}
	if claims.NotBefore != 0 && now.Add(verifier.clockSkew).Before(time.Unix(claims.NotBefore, 0)) {
		return VerifiedIdentity{}, errors.New("identity assertion is not active")
	}
	scopes := uniqueScopes(append(strings.Fields(claims.Scope), claims.Scopes...))
	return VerifiedIdentity{
		ActorType: claims.ActorType, ActorID: claims.Subject, ActingTenantID: claims.ActingTenantID, Scopes: scopes,
	}, nil
}

func ParseRSAPublicKeyPEM(payload []byte) (*rsa.PublicKey, error) {
	block, remainder := pem.Decode(payload)
	if block == nil || len(strings.TrimSpace(string(remainder))) != 0 {
		return nil, errors.New("Human IAM public key must contain one PEM block")
	}
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		publicKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("Human IAM public key is not RSA")
		}
		return publicKey, nil
	}
	if publicKey, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return publicKey, nil
	}
	return nil, errors.New("Human IAM RSA public key PEM is invalid")
}

type assertionClaims struct {
	Issuer         string    `json:"iss"`
	Audience       audiences `json:"aud"`
	Subject        string    `json:"sub"`
	ActorType      string    `json:"actor_type"`
	ActingTenantID string    `json:"acting_tenant_id"`
	Scope          string    `json:"scope"`
	Scopes         []string  `json:"scopes"`
	ExpiresAt      int64     `json:"exp"`
	NotBefore      int64     `json:"nbf"`
}

type audiences []string

func (audience *audiences) UnmarshalJSON(payload []byte) error {
	var single string
	if err := json.Unmarshal(payload, &single); err == nil {
		*audience = audiences{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(payload, &many); err != nil {
		return fmt.Errorf("invalid audience claim: %w", err)
	}
	*audience = many
	return nil
}

func (audience audiences) Contains(wanted string) bool {
	for _, value := range audience {
		if value == wanted {
			return true
		}
	}
	return false
}

func bearerAssertion(authorization string) (string, error) {
	fields := strings.Fields(authorization)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || fields[1] == "" {
		return "", errors.New("Bearer Human IAM assertion is required")
	}
	return fields[1], nil
}

func uniqueScopes(scopes []string) []string {
	result := make([]string, 0, len(scopes))
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if _, duplicate := seen[scope]; duplicate {
			continue
		}
		seen[scope] = struct{}{}
		result = append(result, scope)
	}
	return result
}

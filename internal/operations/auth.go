package operations

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var ErrInvalidGatewayIdentity = errors.New("invalid Gateway identity")
var ErrInvalidMeteringIdentity = errors.New("invalid Metering identity")

type GatewayVerifier interface {
	Verify(context.Context, string, string, string, []byte) (GatewayIdentity, error)
}

type MeteringVerifier interface {
	Verify(context.Context, string, string, string, []byte) (MeteringIdentity, error)
}

type MeteringHMACVerifier struct {
	keys    map[string][]byte
	regions map[string]string
	now     func() time.Time
	maxSkew time.Duration
}

func NewMeteringHMACVerifier(keys, regions map[string]string, now func() time.Time) (*MeteringHMACVerifier, error) {
	if len(keys) == 0 || len(keys) != len(regions) {
		return nil, errors.New("Metering HMAC identities require matching key and region maps")
	}
	if now == nil {
		now = time.Now
	}
	copied := make(map[string][]byte, len(keys))
	regionCopy := make(map[string]string, len(regions))
	for id, key := range keys {
		region := strings.TrimSpace(regions[id])
		if !resourceID.MatchString(id) || len(key) < 32 || region == "" {
			return nil, errors.New("Metering HMAC identities require valid IDs, regions, and 32-byte keys")
		}
		copied[id] = []byte(key)
		regionCopy[id] = region
	}
	return &MeteringHMACVerifier{keys: copied, regions: regionCopy, now: now, maxSkew: 2 * time.Minute}, nil
}

func (verifier *MeteringHMACVerifier) Verify(_ context.Context, authorization, method, requestURI string, body []byte) (MeteringIdentity, error) {
	const prefix = "Metering-HMAC "
	if !strings.HasPrefix(authorization, prefix) {
		return MeteringIdentity{}, ErrInvalidMeteringIdentity
	}
	parts := strings.Split(strings.TrimPrefix(authorization, prefix), ":")
	if len(parts) != 3 {
		return MeteringIdentity{}, ErrInvalidMeteringIdentity
	}
	id, timestampText, signatureText := parts[0], parts[1], parts[2]
	key, exists := verifier.keys[id]
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	signature, signatureErr := hex.DecodeString(signatureText)
	if !exists || err != nil || signatureErr != nil || len(signature) != sha256.Size {
		return MeteringIdentity{}, ErrInvalidMeteringIdentity
	}
	observed := time.Unix(timestamp, 0)
	if skew := verifier.now().UTC().Sub(observed); skew > verifier.maxSkew || skew < -verifier.maxSkew {
		return MeteringIdentity{}, ErrInvalidMeteringIdentity
	}
	expected := componentSignature(key, id, timestampText, method, requestURI, body)
	if !hmac.Equal(signature, expected) {
		return MeteringIdentity{}, ErrInvalidMeteringIdentity
	}
	return MeteringIdentity{MeteringID: id, Region: verifier.regions[id]}, nil
}

func MeteringAuthorization(key []byte, id string, at time.Time, method, requestURI string, body []byte) (string, error) {
	if !resourceID.MatchString(id) || len(key) < 32 || at.IsZero() {
		return "", ErrInvalidMeteringIdentity
	}
	timestamp := strconv.FormatInt(at.UTC().Unix(), 10)
	return fmt.Sprintf("Metering-HMAC %s:%s:%s", id, timestamp, hex.EncodeToString(componentSignature(key, id, timestamp, method, requestURI, body))), nil
}

type HMACVerifier struct {
	keys    map[string][]byte
	regions map[string]string
	now     func() time.Time
	maxSkew time.Duration
}

func NewHMACVerifier(keys, regions map[string]string, now func() time.Time) (*HMACVerifier, error) {
	if len(keys) == 0 || len(keys) != len(regions) {
		return nil, errors.New("Gateway HMAC identities require matching key and region maps")
	}
	if now == nil {
		now = time.Now
	}
	copied := make(map[string][]byte, len(keys))
	regionCopy := make(map[string]string, len(regions))
	for id, key := range keys {
		region := strings.TrimSpace(regions[id])
		if !resourceID.MatchString(id) || len(key) < 32 || region == "" {
			return nil, errors.New("Gateway HMAC identities require valid IDs, regions, and 32-byte keys")
		}
		copied[id] = []byte(key)
		regionCopy[id] = region
	}
	return &HMACVerifier{keys: copied, regions: regionCopy, now: now, maxSkew: 2 * time.Minute}, nil
}

func (verifier *HMACVerifier) Verify(_ context.Context, authorization, method, requestURI string, body []byte) (GatewayIdentity, error) {
	const prefix = "Gateway-HMAC "
	if !strings.HasPrefix(authorization, prefix) {
		return GatewayIdentity{}, ErrInvalidGatewayIdentity
	}
	parts := strings.Split(strings.TrimPrefix(authorization, prefix), ":")
	if len(parts) != 3 {
		return GatewayIdentity{}, ErrInvalidGatewayIdentity
	}
	gatewayID, timestampText, signatureText := parts[0], parts[1], parts[2]
	key, exists := verifier.keys[gatewayID]
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	signature, signatureErr := hex.DecodeString(signatureText)
	if !exists || err != nil || signatureErr != nil || len(signature) != sha256.Size {
		return GatewayIdentity{}, ErrInvalidGatewayIdentity
	}
	observed := time.Unix(timestamp, 0)
	if skew := verifier.now().UTC().Sub(observed); skew > verifier.maxSkew || skew < -verifier.maxSkew {
		return GatewayIdentity{}, ErrInvalidGatewayIdentity
	}
	expected := gatewaySignature(key, gatewayID, timestampText, method, requestURI, body)
	if !hmac.Equal(signature, expected) {
		return GatewayIdentity{}, ErrInvalidGatewayIdentity
	}
	return GatewayIdentity{GatewayID: gatewayID, Region: verifier.regions[gatewayID]}, nil
}

func GatewayAuthorization(key []byte, gatewayID string, at time.Time, method, requestURI string, body []byte) (string, error) {
	if !resourceID.MatchString(gatewayID) || len(key) < 32 || at.IsZero() {
		return "", ErrInvalidGatewayIdentity
	}
	timestamp := strconv.FormatInt(at.UTC().Unix(), 10)
	return fmt.Sprintf("Gateway-HMAC %s:%s:%s", gatewayID, timestamp, hex.EncodeToString(gatewaySignature(key, gatewayID, timestamp, method, requestURI, body))), nil
}

func gatewaySignature(key []byte, gatewayID, timestamp, method, requestURI string, body []byte) []byte {
	return componentSignature(key, gatewayID, timestamp, method, requestURI, body)
}

func componentSignature(key []byte, identity, timestamp, method, requestURI string, body []byte) []byte {
	digest := sha256.Sum256(body)
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n%s", identity, timestamp, method, requestURI, hex.EncodeToString(digest[:]))
	return mac.Sum(nil)
}

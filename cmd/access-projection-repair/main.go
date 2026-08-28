package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/accessprojection"
	"github.com/toddzheng/llm-gateway/internal/credentialadmin"
	"github.com/toddzheng/llm-gateway/internal/dbtransport"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("Gateway Access Projection repair failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	tenantID := strings.TrimSpace(os.Getenv("ACCESS_PROJECTION_REPAIR_TENANT_ID"))
	controlURL := strings.TrimSpace(os.Getenv("CONTROL_PLANE_DATABASE_URL"))
	gatewayURL := strings.TrimSpace(os.Getenv("GATEWAY_DATABASE_URL"))
	if tenantID == "" || controlURL == "" || gatewayURL == "" {
		return errors.New("ACCESS_PROJECTION_REPAIR_TENANT_ID, CONTROL_PLANE_DATABASE_URL, and GATEWAY_DATABASE_URL are required")
	}
	if err := dbtransport.RequireAuthenticatedEncryption(controlURL); err != nil {
		return fmt.Errorf("authoritative control database transport: %w", err)
	}
	if err := dbtransport.RequireAuthenticatedEncryption(gatewayURL); err != nil {
		return fmt.Errorf("Gateway projection database transport: %w", err)
	}
	ring, err := pepperRingFromEnv()
	if err != nil {
		return err
	}
	controlDatabase, err := openDatabase(ctx, controlURL)
	if err != nil {
		return fmt.Errorf("connect authoritative control database: %w", err)
	}
	defer controlDatabase.Close()
	gatewayDatabase, err := openDatabase(ctx, gatewayURL)
	if err != nil {
		return fmt.Errorf("connect Gateway projection database: %w", err)
	}
	defer gatewayDatabase.Close()
	credentials, err := credentialadmin.NewService(controlDatabase, credentialadmin.PepperRing(ring), time.Now, nil)
	if err != nil {
		return err
	}
	snapshot, err := credentials.BuildAccessSnapshot(ctx, tenantadmin.ActorEnvelope{
		Type: "system", ID: "gateway-access-projection-repair", Scopes: []string{tenantadmin.ScopePlatformRead},
	}, tenantID)
	if err != nil {
		return fmt.Errorf("build authoritative access snapshot: %w", err)
	}
	for _, key := range snapshot.Keys {
		if snapshot.Tenant.Status != access.TenantClosed && key.Status == access.APIKeyActive &&
			(key.ExpiresAt == nil || key.ExpiresAt.After(time.Now().UTC())) &&
			len(ring.Peppers[key.DigestVersion]) == 0 {
			return fmt.Errorf("snapshot requires unconfigured digest pepper version %d", key.DigestVersion)
		}
	}
	projection, err := accessprojection.New(gatewayDatabase, ring, time.Now)
	if err != nil {
		return err
	}
	if err := projection.ReplaceSnapshot(ctx, snapshot); err != nil {
		return fmt.Errorf("atomically replace Gateway Access Projection: %w", err)
	}
	if err := projection.ValidatePepperCoverage(ctx); err != nil {
		return fmt.Errorf("validate repaired projection pepper coverage: %w", err)
	}
	slog.Info("Gateway Access Projection repaired", "tenant_id", tenantID, "tenant_revision", snapshot.Tenant.Revision, "key_count", len(snapshot.Keys))
	return nil
}

func openDatabase(ctx context.Context, databaseURL string) (*sql.DB, error) {
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	connectContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := database.PingContext(connectContext); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

func pepperRingFromEnv() (accessprojection.PepperRing, error) {
	encoded := strings.TrimSpace(os.Getenv("CONTROL_API_KEY_PEPPERS_JSON"))
	currentValue := strings.TrimSpace(os.Getenv("CONTROL_API_KEY_CURRENT_DIGEST_VERSION"))
	if encoded == "" || currentValue == "" {
		return accessprojection.PepperRing{}, errors.New("CONTROL_API_KEY_PEPPERS_JSON and CONTROL_API_KEY_CURRENT_DIGEST_VERSION are required")
	}
	var configured map[string]string
	if err := json.Unmarshal([]byte(encoded), &configured); err != nil {
		return accessprojection.PepperRing{}, fmt.Errorf("CONTROL_API_KEY_PEPPERS_JSON: %w", err)
	}
	current, err := strconv.ParseInt(currentValue, 10, 16)
	if err != nil || current <= 0 {
		return accessprojection.PepperRing{}, errors.New("CONTROL_API_KEY_CURRENT_DIGEST_VERSION must be a positive integer")
	}
	peppers := make(map[int16][]byte, len(configured))
	for versionValue, pepper := range configured {
		version, err := strconv.ParseInt(versionValue, 10, 16)
		if err != nil || version <= 0 {
			return accessprojection.PepperRing{}, errors.New("CONTROL_API_KEY_PEPPERS_JSON keys must be positive digest versions")
		}
		peppers[int16(version)] = []byte(pepper)
	}
	return accessprojection.PepperRing{CurrentVersion: int16(current), Peppers: peppers}, nil
}

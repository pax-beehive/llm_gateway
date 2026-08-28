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
	"github.com/toddzheng/llm-gateway/internal/accessbootstrap"
	"github.com/toddzheng/llm-gateway/internal/accessprojection"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/credentialadmin"
	"github.com/toddzheng/llm-gateway/internal/migrations"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("access bootstrap failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	databaseURL := strings.TrimSpace(os.Getenv("GATEWAY_DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("GATEWAY_DATABASE_URL is required")
	}
	if os.Getenv("GATEWAY_ENV") == "production" {
		return errors.New("environment access bootstrap is development-only; use the control plane for production Tenant and key issuance")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		return err
	}
	if os.Getenv("GATEWAY_MIGRATE") == "true" {
		if err := migrations.Migrate(ctx, database); err != nil {
			return err
		}
	}
	current, peppers, err := pepperRingFromEnv()
	if err != nil {
		return err
	}
	service, err := access.NewPostgresServiceWithPeppers(database, current, peppers)
	if err != nil {
		return err
	}
	apiKeys, err := parseMap[string]("GATEWAY_API_KEYS_JSON")
	if err != nil {
		return err
	}
	homeRegions, err := parseMap[string]("GATEWAY_TENANT_HOME_REGIONS_JSON")
	if err != nil {
		return err
	}
	epochs, err := parseMap[int64]("GATEWAY_TENANT_EXECUTION_EPOCHS_JSON")
	if err != nil {
		return err
	}
	tenantPolicies, err := parseMap[core.TenantPolicy]("GATEWAY_TENANT_POLICIES_JSON")
	if err != nil {
		return err
	}
	keyPolicies, err := parseMap[core.APIKeyPolicy]("GATEWAY_API_KEY_POLICIES_JSON")
	if err != nil {
		return err
	}
	metadata, err := parseMap[map[string]any]("GATEWAY_API_KEY_METADATA_JSON")
	if err != nil {
		return err
	}
	tenantIDs, err := accessbootstrap.Bootstrap(ctx, service, accessbootstrap.Input{APIKeys: apiKeys, HomeRegions: homeRegions, ExecutionEpochs: epochs, TenantPolicies: tenantPolicies, APIKeyPolicies: keyPolicies, APIKeyMetadata: metadata})
	if err != nil {
		return err
	}
	if os.Getenv("GATEWAY_ACCESS_PROJECTION") == "true" {
		if os.Getenv("GATEWAY_MIGRATE") == "true" {
			if err := accessprojection.Migrate(ctx, database); err != nil {
				return err
			}
		}
		projection, err := accessprojection.New(database, accessprojection.PepperRing{CurrentVersion: current, Peppers: peppers}, time.Now)
		if err != nil {
			return err
		}
		credentials, err := credentialadmin.NewService(database, credentialadmin.PepperRing{CurrentVersion: current, Peppers: peppers}, time.Now, nil)
		if err != nil {
			return err
		}
		actor := tenantadmin.ActorEnvelope{Type: "system", ID: "access-bootstrap", Scopes: []string{tenantadmin.ScopePlatformRead}}
		for _, tenantID := range tenantIDs {
			snapshot, err := credentials.BuildAccessSnapshot(ctx, actor, tenantID)
			if err != nil {
				return err
			}
			if err := projection.ReplaceSnapshot(ctx, snapshot); err != nil {
				return err
			}
		}
	}
	slog.Info("access bootstrap completed", "tenant_count", len(tenantIDs), "key_count", len(apiKeys))
	return nil
}

func parseMap[T any](name string) (map[string]T, error) {
	result := map[string]T{}
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return result, nil
	}
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return result, nil
}

func pepperRingFromEnv() (int16, map[int16][]byte, error) {
	encoded := strings.TrimSpace(os.Getenv("GATEWAY_API_KEY_PEPPERS_JSON"))
	if encoded == "" {
		pepper := []byte(os.Getenv("GATEWAY_API_KEY_PEPPER"))
		if len(pepper) < 16 {
			return 0, nil, errors.New("GATEWAY_API_KEY_PEPPER must contain at least 16 bytes")
		}
		return 1, map[int16][]byte{1: pepper}, nil
	}
	var configured map[string]string
	if err := json.Unmarshal([]byte(encoded), &configured); err != nil {
		return 0, nil, err
	}
	currentValue, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("GATEWAY_API_KEY_CURRENT_DIGEST_VERSION")), 10, 16)
	if err != nil || currentValue <= 0 {
		return 0, nil, errors.New("GATEWAY_API_KEY_CURRENT_DIGEST_VERSION must be positive")
	}
	peppers := make(map[int16][]byte, len(configured))
	for versionValue, pepper := range configured {
		version, err := strconv.ParseInt(versionValue, 10, 16)
		if err != nil || version <= 0 || len(pepper) < 16 {
			return 0, nil, errors.New("Gateway API key peppers require positive versions and at least 16 bytes")
		}
		peppers[int16(version)] = []byte(pepper)
	}
	return int16(currentValue), peppers, nil
}

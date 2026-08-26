package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/toddzheng/llm-gateway/internal/controlapi"
	"github.com/toddzheng/llm-gateway/internal/store"
	"github.com/toddzheng/llm-gateway/internal/telemetry"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func main() {
	if err := run(); err != nil {
		slog.Error("control plane stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownTelemetry, err := telemetry.Configure(ctx, envOr("OTEL_SERVICE_NAME", "llm-gateway-control-plane"))
	if err != nil {
		return fmt.Errorf("configure OpenTelemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			slog.Warn("OpenTelemetry shutdown failed", "error", err)
		}
	}()
	devMode := os.Getenv("CONTROL_PLANE_DEV_MODE") == "true"
	databaseURL := strings.TrimSpace(os.Getenv("CONTROL_PLANE_DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("CONTROL_PLANE_DATABASE_URL is required")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	database.SetMaxOpenConns(20)
	database.SetMaxIdleConns(5)
	database.SetConnMaxLifetime(30 * time.Minute)
	connectCtx, cancelConnect := context.WithTimeout(ctx, 10*time.Second)
	defer cancelConnect()
	if err := database.PingContext(connectCtx); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	if err := assertDatabaseRole(connectCtx, database, devMode); err != nil {
		return err
	}
	if os.Getenv("CONTROL_PLANE_MIGRATE") == "true" {
		if !devMode {
			return errors.New("CONTROL_PLANE_MIGRATE is development-only; production migrations require a separate deployment gate")
		}
		if err := store.NewPostgresResponseStore(database).Migrate(ctx); err != nil {
			return fmt.Errorf("migrate shared schema: %w", err)
		}
		if err := tenantadmin.Migrate(ctx, database); err != nil {
			return err
		}
	}
	administration, err := tenantadmin.NewService(database, time.Now)
	if err != nil {
		return err
	}
	verifier, err := configureIdentityVerifier()
	if err != nil {
		return err
	}
	api := controlapi.New(controlapi.Config{Administration: administration, Verifier: verifier})
	mux := http.NewServeMux()
	mux.Handle("/", api)
	address := envOr("CONTROL_PLANE_ADDR", ":8081")
	server := &http.Server{
		Addr: address, Handler: otelhttp.NewHandler(mux, "control-plane.http"),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	serveErrors := make(chan error, 1)
	go func() {
		slog.Info("control plane listening", "address", address)
		serveErrors <- server.ListenAndServe()
	}()
	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func configureIdentityVerifier() (controlapi.IdentityVerifier, error) {
	if os.Getenv("CONTROL_PLANE_DEV_MODE") == "true" {
		token := os.Getenv("CONTROL_PLANE_DEV_TOKEN")
		if token == "" || len(token) < 16 {
			return nil, errors.New("CONTROL_PLANE_DEV_TOKEN must contain at least 16 characters in development mode")
		}
		return controlapi.IdentityVerifierFunc(func(_ context.Context, authorization string) (controlapi.VerifiedIdentity, error) {
			fields := strings.Fields(authorization)
			if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || subtle.ConstantTimeCompare([]byte(fields[1]), []byte(token)) != 1 {
				return controlapi.VerifiedIdentity{}, errors.New("invalid development identity assertion")
			}
			return controlapi.VerifiedIdentity{
				ActorType: "human", ActorID: "dev-operator",
				Scopes: []string{tenantadmin.ScopePlatformRead, tenantadmin.ScopePlatformWrite},
			}, nil
		}), nil
	}
	jwksURL := strings.TrimSpace(os.Getenv("CONTROL_IAM_JWKS_URL"))
	issuer := strings.TrimSpace(os.Getenv("CONTROL_IAM_ISSUER"))
	audience := strings.TrimSpace(os.Getenv("CONTROL_IAM_AUDIENCE"))
	if jwksURL == "" || issuer == "" || audience == "" {
		return nil, errors.New("CONTROL_IAM_JWKS_URL, CONTROL_IAM_ISSUER, and CONTROL_IAM_AUDIENCE are required")
	}
	return controlapi.NewJWKSVerifier(controlapi.JWKSVerifierConfig{
		URL: jwksURL, Issuer: issuer, Audience: audience, ClockSkew: 30 * time.Second,
	})
}

func assertDatabaseRole(ctx context.Context, database *sql.DB, devMode bool) error {
	if devMode {
		return nil
	}
	expected := strings.TrimSpace(os.Getenv("CONTROL_PLANE_DB_ROLE"))
	if expected == "" {
		return errors.New("CONTROL_PLANE_DB_ROLE is required outside development mode")
	}
	var current string
	if err := database.QueryRowContext(ctx, `SELECT current_user`).Scan(&current); err != nil {
		return fmt.Errorf("inspect control-plane database role: %w", err)
	}
	if current != expected {
		return fmt.Errorf("control-plane database role mismatch: connected as %q", current)
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

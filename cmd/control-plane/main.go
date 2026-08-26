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
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
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
	if os.Getenv("CONTROL_PLANE_MIGRATE") == "true" {
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
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, request *http.Request) {
		readyCtx, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		if err := database.PingContext(readyCtx); err != nil {
			http.Error(writer, `{"status":"not_ready"}`, http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte("{\"status\":\"ready\"}\n"))
	})
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
	keyFile := strings.TrimSpace(os.Getenv("CONTROL_IAM_PUBLIC_KEY_FILE"))
	issuer := strings.TrimSpace(os.Getenv("CONTROL_IAM_ISSUER"))
	audience := strings.TrimSpace(os.Getenv("CONTROL_IAM_AUDIENCE"))
	if keyFile == "" || issuer == "" || audience == "" {
		return nil, errors.New("CONTROL_IAM_PUBLIC_KEY_FILE, CONTROL_IAM_ISSUER, and CONTROL_IAM_AUDIENCE are required")
	}
	payload, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read Human IAM public key: %w", err)
	}
	publicKey, err := controlapi.ParseRSAPublicKeyPEM(payload)
	if err != nil {
		return nil, err
	}
	return controlapi.NewRS256Verifier(controlapi.RS256VerifierConfig{
		PublicKey: publicKey, Issuer: issuer, Audience: audience, ClockSkew: 30 * time.Second,
	})
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

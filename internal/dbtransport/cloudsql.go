package dbtransport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var cloudSQLInstancePart = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// RequireAuthenticatedTransport accepts either a PostgreSQL connection whose
// driver performs certificate and hostname verification or an explicit Cloud
// SQL Connector instance. The Go Connector authenticates both endpoints and
// establishes TLS independently of the PostgreSQL protocol.
func RequireAuthenticatedTransport(databaseURL, cloudSQLInstance string) error {
	if strings.TrimSpace(cloudSQLInstance) == "" {
		return RequireAuthenticatedEncryption(databaseURL)
	}
	return validateCloudSQLInstance(cloudSQLInstance)
}

// Open opens PostgreSQL directly when cloudSQLInstance is empty. Otherwise it
// uses the Cloud SQL Go Connector and returns a cleanup function for the
// connector and registered pgx configuration. The caller still owns db.Close.
func Open(ctx context.Context, databaseURL, cloudSQLInstance string) (*sql.DB, func() error, error) {
	instance := strings.TrimSpace(cloudSQLInstance)
	if instance == "" {
		database, err := sql.Open("pgx", databaseURL)
		return database, func() error { return nil }, err
	}
	if err := validateCloudSQLInstance(instance); err != nil {
		return nil, func() error { return nil }, err
	}

	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, func() error { return nil }, fmt.Errorf("parse Cloud SQL PostgreSQL configuration: %w", err)
	}
	if len(config.Fallbacks) != 0 {
		return nil, func() error { return nil }, errors.New("Cloud SQL Connector configuration must not contain fallback hosts")
	}

	dialer, err := cloudsqlconn.NewDialer(ctx, cloudsqlconn.WithLazyRefresh())
	if err != nil {
		return nil, func() error { return nil }, fmt.Errorf("create Cloud SQL Connector: %w", err)
	}
	config.TLSConfig = nil
	config.DialFunc = func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return dialer.Dial(dialCtx, instance)
	}
	registered := stdlib.RegisterConnConfig(config)
	database, err := sql.Open("pgx", registered)
	if err != nil {
		stdlib.UnregisterConnConfig(registered)
		_ = dialer.Close()
		return nil, func() error { return nil }, err
	}
	cleanup := func() error {
		stdlib.UnregisterConnConfig(registered)
		return dialer.Close()
	}
	return database, cleanup, nil
}

func validateCloudSQLInstance(instance string) error {
	parts := strings.Split(strings.TrimSpace(instance), ":")
	if len(parts) != 3 || !cloudSQLInstancePart.MatchString(parts[0]) ||
		!cloudSQLInstancePart.MatchString(parts[1]) || !cloudSQLInstancePart.MatchString(parts[2]) {
		return errors.New("Cloud SQL instance must be project:region:instance using lowercase resource names")
	}
	return nil
}

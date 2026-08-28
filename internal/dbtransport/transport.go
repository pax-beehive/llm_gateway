package dbtransport

import (
	"crypto/tls"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func RequireAuthenticatedEncryption(databaseURL string) error {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse PostgreSQL transport configuration: %w", err)
	}
	if !authenticatedTLS(config.TLSConfig) {
		return errors.New("PostgreSQL transport must use certificate-verified TLS")
	}
	for _, fallback := range config.Fallbacks {
		if !authenticatedTLS(fallback.TLSConfig) {
			return errors.New("PostgreSQL transport fallback must use certificate-verified TLS")
		}
	}
	return nil
}

func authenticatedTLS(config *tls.Config) bool {
	return config != nil && !config.InsecureSkipVerify && config.ServerName != ""
}

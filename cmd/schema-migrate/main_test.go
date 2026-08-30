package main

import (
	"strings"
	"testing"
)

func TestConfigurationFromEnvironment(t *testing.T) {
	t.Setenv("SCHEMA_MIGRATION_CONFIRM", "apply")
	t.Setenv("ADMIN_DATABASE_URL", "user=admin dbname=llm_gateway")
	t.Setenv("SCHEMA_MIGRATION_CLOUD_SQL_INSTANCE", "pax-fde-prod:us-west1:llm-gateway-prod-postgres")

	configuration, err := configurationFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if configuration.databaseURL != "user=admin dbname=llm_gateway" ||
		configuration.cloudSQLInstance != "pax-fde-prod:us-west1:llm-gateway-prod-postgres" {
		t.Fatalf("configuration = %#v", configuration)
	}
}

func TestConfigurationRequiresExplicitConfirmation(t *testing.T) {
	t.Setenv("ADMIN_DATABASE_URL", "user=admin dbname=llm_gateway")
	_, err := configurationFromEnvironment()
	if err == nil || !strings.Contains(err.Error(), "SCHEMA_MIGRATION_CONFIRM") {
		t.Fatalf("error = %v", err)
	}
}

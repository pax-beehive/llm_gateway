//go:build integration

package controlaudit_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/toddzheng/llm-gateway/internal/controlaudit"
	"github.com/toddzheng/llm-gateway/internal/migrations"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestAuditListIsStableFilteredAndTenantIsolated(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := migrations.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := tenantadmin.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("audit-query-%d", time.Now().UnixNano())
	tenantID := prefix + "-tenant"
	if _, err := database.ExecContext(ctx, `INSERT INTO tenants(id,home_region,policy_revision,policy) VALUES($1,'test',1,'{"revision":1}')`, tenantID); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute)
	for index := 0; index < 3; index++ {
		if _, err := database.ExecContext(ctx, `INSERT INTO control_audit_events(event_id,tenant_id,actor_type,actor_id,scopes,request_id,reason,action,aggregate_type,aggregate_id,aggregate_revision,payload,occurred_at)
			VALUES($1,$2,'human','operator-a',$3,$4,'integration test','tenant.update','Tenant',$2,$5,'{}',$6)`,
			fmt.Sprintf("%s-%d", prefix, index), tenantID, []string{tenantadmin.ScopePlatformRead}, fmt.Sprintf("request-%d", index), index+1, base.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	service, err := controlaudit.NewService(database)
	if err != nil {
		t.Fatal(err)
	}
	platform := tenantadmin.ActorEnvelope{Type: "human", ID: "platform-reader", Scopes: []string{tenantadmin.ScopePlatformRead}}
	first, err := service.List(ctx, platform, controlaudit.Filter{TenantID: tenantID, Action: "tenant.update", Limit: 2})
	if err != nil || len(first.Data) != 2 || first.NextCursor == "" || !first.Data[0].OccurredAt.After(first.Data[1].OccurredAt) {
		t.Fatalf("first page = %#v err=%v", first, err)
	}
	second, err := service.List(ctx, platform, controlaudit.Filter{TenantID: tenantID, Action: "tenant.update", Cursor: first.NextCursor, Limit: 2})
	if err != nil || len(second.Data) != 1 || second.NextCursor != "" || second.Data[0].ID == first.Data[1].ID {
		t.Fatalf("second page = %#v err=%v", second, err)
	}
	tenantReader := tenantadmin.ActorEnvelope{Type: "human", ID: "tenant-reader", ActingTenantID: tenantID, Scopes: []string{tenantadmin.ScopeTenantRead}}
	if _, err := service.List(ctx, tenantReader, controlaudit.Filter{TenantID: "another-tenant"}); err != controlaudit.ErrPolicyDenied {
		t.Fatalf("cross-Tenant audit error = %v", err)
	}
}

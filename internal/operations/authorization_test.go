package operations

import (
	"testing"

	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestWorkOSOperationsReadPermissionIsAccepted(t *testing.T) {
	if !canRead(tenantadmin.ActorEnvelope{Type: "human", ID: "user_1", Scopes: []string{ScopePlatformRead}}) {
		t.Fatal("platform:operations:read was denied")
	}
}

package routingcatalog

import (
	"testing"

	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestWorkOSPermissionVocabularyAuthorizesRoutingCatalog(t *testing.T) {
	for _, scope := range []string{ScopePlatformRead, ScopeGatewayModels} {
		if err := authorizeCatalogRead(tenantadmin.ActorEnvelope{Type: "human", ID: "user_1", Scopes: []string{scope}}); err != nil {
			t.Fatalf("routing read scope %q denied: %v", scope, err)
		}
	}
	actor := tenantadmin.ActorEnvelope{Type: "human", ID: "user_1", RequestID: "req_1", Reason: "publish routing", Scopes: []string{ScopePlatformWrite}}
	if err := authorizeCatalogWrite(actor); err != nil {
		t.Fatalf("routing write scope denied: %v", err)
	}
}

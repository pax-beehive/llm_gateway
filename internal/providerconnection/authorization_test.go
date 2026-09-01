package providerconnection

import (
	"testing"

	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func TestWorkOSPermissionVocabularyAuthorizesProviderOperations(t *testing.T) {
	for _, scope := range []string{ScopePlatformRead, ScopeRoutingRead, ScopeGatewayModels} {
		if err := authorizeRead(tenantadmin.ActorEnvelope{Type: "human", ID: "user_1", Scopes: []string{scope}}); err != nil {
			t.Fatalf("read scope %q denied: %v", scope, err)
		}
	}
	actor := tenantadmin.ActorEnvelope{Type: "human", ID: "user_1", RequestID: "req_1", Reason: "update provider", Scopes: []string{ScopePlatformWrite}}
	if err := authorizeMutation(actor); err != nil {
		t.Fatalf("provider write scope denied: %v", err)
	}
}

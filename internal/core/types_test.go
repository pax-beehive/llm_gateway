package core_test

import (
	"encoding/json"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/core"
)

func TestFunctionCallArgumentsUseResponsesStringWireShape(t *testing.T) {
	t.Parallel()
	item := core.Item{Type: "function_call", CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"q":"go"}`)}
	payload, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"q\":\"go\"}"}` {
		t.Fatalf("wire item = %s", payload)
	}
	var decoded core.Item
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded.Arguments) != `{"q":"go"}` {
		t.Fatalf("decoded arguments = %s", decoded.Arguments)
	}
}

func TestAPIKeyPolicyPreservesMissingInheritAndExplicitDeny(t *testing.T) {
	t.Parallel()
	var inherited core.APIKeyPolicy
	if err := json.Unmarshal([]byte(`{}`), &inherited); err != nil {
		t.Fatal(err)
	}
	if inherited.AllowedPublicModels != nil || inherited.AllowedOperations != nil || inherited.AllowedCIDRs != nil ||
		inherited.AllowedRegions != nil || inherited.MaxConcurrentResponses != nil {
		t.Fatalf("missing restrictions must inherit: %#v", inherited)
	}

	var denied core.APIKeyPolicy
	if err := json.Unmarshal([]byte(`{"allowed_public_models":[],"allowed_operations":[],"allowed_cidrs":[],"allowed_regions":[],"max_concurrent_responses":0}`), &denied); err != nil {
		t.Fatal(err)
	}
	if denied.AllowedPublicModels == nil || len(*denied.AllowedPublicModels) != 0 ||
		denied.AllowedOperations == nil || len(*denied.AllowedOperations) != 0 ||
		denied.AllowedCIDRs == nil || len(*denied.AllowedCIDRs) != 0 ||
		denied.AllowedRegions == nil || len(*denied.AllowedRegions) != 0 ||
		denied.MaxConcurrentResponses == nil || *denied.MaxConcurrentResponses != 0 {
		t.Fatalf("explicit empty and zero restrictions must deny: %#v", denied)
	}
}

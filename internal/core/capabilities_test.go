package core_test

import (
	"encoding/json"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/core"
)

func TestCapabilityProviderUsageMustRemainContentFree(t *testing.T) {
	t.Parallel()
	for _, valid := range []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"input_tokens":7,"details":{"cached_tokens":2},"synthetic":true}`),
		json.RawMessage(`{"billed_units":[1,2,3]}`),
	} {
		if err := core.ValidateCapabilityProviderUsage(valid); err != nil {
			t.Fatalf("valid usage %s: %v", valid, err)
		}
	}
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`{"prompt":"secret text"}`),
		json.RawMessage(`{"details":[{"document":"secret"}]}`),
		json.RawMessage(`[]`),
		json.RawMessage(`not-json`),
	} {
		if err := core.ValidateCapabilityProviderUsage(invalid); err == nil {
			t.Fatalf("invalid usage accepted: %s", invalid)
		}
	}
}

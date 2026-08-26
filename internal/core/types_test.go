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

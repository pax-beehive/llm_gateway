package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseProviderInputRequiresCompleteAllowlistedBundle(t *testing.T) {
	input := map[string]string{
		"OPENAI_API_KEY": "test-openai", "DEEPSEEK_API_KEY": "test-deepseek",
		"ANTHROPIC_API_KEY": "test-anthropic", "GEMINI_API_KEY": "test-gemini",
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	specs, canary, err := parseProviderInput(string(encoded), "openai")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 4 || canary != "openai" {
		t.Fatalf("specs=%d canary=%q", len(specs), canary)
	}

	delete(input, "GEMINI_API_KEY")
	encoded, _ = json.Marshal(input)
	if _, _, err := parseProviderInput(string(encoded), "openai"); err == nil {
		t.Fatal("incomplete Provider bundle was accepted")
	}
	input["GEMINI_API_KEY"] = "test-gemini"
	input["UNSUPPORTED_API_KEY"] = "test-unsupported"
	encoded, _ = json.Marshal(input)
	if _, _, err := parseProviderInput(string(encoded), "openai"); err == nil {
		t.Fatal("unsupported Provider key was accepted")
	}
}

func TestParseProviderInputRejectsUnknownCanaryWithoutCredentialEcho(t *testing.T) {
	encoded := `{"OPENAI_API_KEY":"secret-openai","DEEPSEEK_API_KEY":"secret-deepseek","ANTHROPIC_API_KEY":"secret-anthropic","GEMINI_API_KEY":"secret-gemini"}`
	_, _, err := parseProviderInput(encoded, "unsupported")
	if err == nil {
		t.Fatal("unsupported canary was accepted")
	}
	for _, secret := range []string{"secret-openai", "secret-deepseek", "secret-anthropic", "secret-gemini"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposed credential material: %v", err)
		}
	}
}

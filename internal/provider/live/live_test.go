//go:build live

package live_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/provider/anthropic"
	"github.com/toddzheng/llm-gateway/internal/provider/modeldiscovery"
	"github.com/toddzheng/llm-gateway/internal/provider/openaicompat"
)

type liveProvider struct {
	name             string
	provider         modeldiscovery.Provider
	apiKey           string
	discoveryBaseURL string
	newExecutor      func(*testing.T, string) provider.ResponseExecutor
}

func TestLiveProviderTextStreaming(t *testing.T) {
	required := []string{
		"OPENAI_API_KEY",
		"DEEPSEEK_API_KEY",
		"ANTHROPIC_API_KEY",
		"GEMINI_API_KEY",
	}
	var missing []string
	for _, name := range required {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("live provider test requires non-empty variables: %s", strings.Join(missing, ", "))
	}

	providers := []liveProvider{
		{
			name: "OpenAI", provider: modeldiscovery.OpenAI, apiKey: os.Getenv("OPENAI_API_KEY"), discoveryBaseURL: "https://api.openai.com/v1",
			newExecutor: func(t *testing.T, model string) provider.ResponseExecutor {
				return mustOpenAICompatible(t, openaicompat.Config{
					BaseURL: "https://api.openai.com/v1", APIKey: os.Getenv("OPENAI_API_KEY"), Model: model, Dialect: openaicompat.DialectOpenAI,
				})
			},
		},
		{
			name: "DeepSeek", provider: modeldiscovery.DeepSeek, apiKey: os.Getenv("DEEPSEEK_API_KEY"), discoveryBaseURL: "https://api.deepseek.com",
			newExecutor: func(t *testing.T, model string) provider.ResponseExecutor {
				return mustOpenAICompatible(t, openaicompat.Config{
					BaseURL: "https://api.deepseek.com", APIKey: os.Getenv("DEEPSEEK_API_KEY"), Model: model, Dialect: openaicompat.DialectDeepSeek,
				})
			},
		},
		{
			name: "Anthropic", provider: modeldiscovery.Anthropic, apiKey: os.Getenv("ANTHROPIC_API_KEY"), discoveryBaseURL: "https://api.anthropic.com/v1",
			newExecutor: func(t *testing.T, model string) provider.ResponseExecutor {
				return mustAnthropic(t, model)
			},
		},
		{
			name: "Gemini", provider: modeldiscovery.Gemini, apiKey: os.Getenv("GEMINI_API_KEY"), discoveryBaseURL: "https://generativelanguage.googleapis.com/v1beta",
			newExecutor: func(t *testing.T, model string) provider.ResponseExecutor {
				return mustOpenAICompatible(t, openaicompat.Config{
					BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", APIKey: os.Getenv("GEMINI_API_KEY"), Model: model, Dialect: openaicompat.DialectGemini,
				})
			},
		},
	}

	for _, live := range providers {
		live := live
		t.Run(live.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			discovery, err := modeldiscovery.New(modeldiscovery.Config{
				Provider: live.provider, BaseURL: live.discoveryBaseURL, APIKey: live.apiKey,
			})
			if err != nil {
				t.Fatal(err)
			}
			models, err := discovery.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			model, err := modeldiscovery.SelectSmokeModel(live.provider, models)
			if err != nil {
				t.Fatal(err)
			}
			executor := live.newExecutor(t, model)
			maxTokens := 64
			stream, err := executor.Execute(ctx, core.Request{
				Input:           []core.Item{{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: "Reply with exactly: pong"}}}},
				MaxOutputTokens: &maxTokens,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			var output strings.Builder
			var usage *core.Usage
			for {
				event, recvErr := stream.Recv()
				if recvErr == io.EOF {
					break
				}
				if recvErr != nil {
					t.Fatal(recvErr)
				}
				output.WriteString(event.Delta)
				if event.Usage != nil {
					usage = event.Usage
				}
			}
			if strings.TrimSpace(output.String()) == "" {
				t.Fatal("provider stream returned no text")
			}
			if usage == nil || usage.TotalTokens <= 0 {
				t.Fatalf("provider stream returned no usable token accounting: %#v", usage)
			}
		})
	}
}

func mustOpenAICompatible(t *testing.T, config openaicompat.Config) provider.ResponseExecutor {
	t.Helper()
	executor, err := openaicompat.New(config)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func mustAnthropic(t *testing.T, model string) provider.ResponseExecutor {
	t.Helper()
	adapter, err := anthropic.NewAdapter(anthropic.AdapterConfig{
		BaseURL: "https://api.anthropic.com/v1",
		APIKey:  os.Getenv("ANTHROPIC_API_KEY"), APIVersion: "2023-06-01", Model: model,
		RouteID: "live-anthropic", CredentialScope: "live-smoke", Region: "global", EnablePromptCaching: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/toddzheng/llm-gateway/internal/access"
	"github.com/toddzheng/llm-gateway/internal/capability"
	"github.com/toddzheng/llm-gateway/internal/core"
	"github.com/toddzheng/llm-gateway/internal/provider"
	"github.com/toddzheng/llm-gateway/internal/runtime"
	"github.com/toddzheng/llm-gateway/internal/store"
)

type Authenticator interface {
	Authenticate(context.Context, string) (access.Principal, error)
}

type APIKeyConcurrencyStore interface {
	AcquireAPIKeyResponseSlot(context.Context, string, string, int, time.Time) error
	RenewAPIKeyResponseSlot(context.Context, string, string, time.Time) error
	ReleaseAPIKeyResponseSlot(context.Context, string, string) error
}

type StaticAuthenticator map[string]string

func (a StaticAuthenticator) Authenticate(_ context.Context, token string) (access.Principal, error) {
	tenantID, ok := a[token]
	if !ok || tenantID == "" {
		return access.Principal{}, errors.New("invalid bearer token")
	}
	return access.Principal{TenantID: tenantID}, nil
}

type principalContextKey struct{}
type apiKeyConcurrencyLeaseContextKey struct{}

var errAPIKeyConcurrencyDenied = errors.New("Gateway API Key concurrent Response policy denies this operation")

type apiKeyConcurrencyLease struct {
	mu                sync.Mutex
	detached          bool
	once              sync.Once
	release           func()
	backgroundContext context.Context
}

func (lease *apiKeyConcurrencyLease) detach() {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	lease.detached = true
}

func (lease *apiKeyConcurrencyLease) releaseAtRequestEnd() {
	lease.mu.Lock()
	detached := lease.detached
	lease.mu.Unlock()
	if !detached {
		lease.finish()
	}
}

func (lease *apiKeyConcurrencyLease) finish() {
	lease.once.Do(lease.release)
}

type Config struct {
	Runtime                        *runtime.Runtime
	CapabilityRuntime              *capability.Runtime
	CapabilityCatalog              provider.CapabilityCatalog
	ModelCatalog                   provider.ModelCatalog
	Authenticator                  Authenticator
	TenantHomeRegions              map[string]string
	TenantExecutionEpochs          map[string]int64
	LocalRegion                    string
	HomeRegionURLs                 map[string]string
	ForwardClient                  *http.Client
	TrustedProxyCIDRs              []string
	APIKeyConcurrencyLeaseTTL      time.Duration
	APIKeyConcurrencyRenewInterval time.Duration
}

type Server struct {
	runtime             *runtime.Runtime
	capabilityRuntime   *capability.Runtime
	capabilityCatalog   provider.CapabilityCatalog
	modelCatalog        provider.ModelCatalog
	authenticator       Authenticator
	homeRegions         map[string]string
	executionEpochs     map[string]int64
	localRegion         string
	homeRegionURLs      map[string]string
	forwardClient       *http.Client
	trustedProxies      []netip.Prefix
	policyConfigError   error
	inflightMu          sync.Mutex
	apiKeyInflight      map[string]int
	apiKeyLeaseTTL      time.Duration
	apiKeyRenewInterval time.Duration
	mux                 *http.ServeMux
}

func New(config Config) http.Handler {
	if config.LocalRegion == "" {
		config.LocalRegion = "local"
	}
	if config.ForwardClient == nil {
		config.ForwardClient = http.DefaultClient
	}
	if config.APIKeyConcurrencyLeaseTTL == 0 {
		config.APIKeyConcurrencyLeaseTTL = 30 * time.Second
	}
	if config.APIKeyConcurrencyRenewInterval == 0 {
		config.APIKeyConcurrencyRenewInterval = 10 * time.Second
	}
	trustedProxies, policyConfigError := parseTrustedProxyCIDRs(config.TrustedProxyCIDRs)
	if config.APIKeyConcurrencyLeaseTTL <= 0 || config.APIKeyConcurrencyRenewInterval <= 0 ||
		config.APIKeyConcurrencyRenewInterval >= config.APIKeyConcurrencyLeaseTTL {
		policyConfigError = errors.New("Gateway API Key concurrency lease timing is invalid")
	}
	server := &Server{
		runtime: config.Runtime, capabilityRuntime: config.CapabilityRuntime, capabilityCatalog: config.CapabilityCatalog,
		modelCatalog: config.ModelCatalog, authenticator: config.Authenticator, homeRegions: config.TenantHomeRegions,
		executionEpochs: config.TenantExecutionEpochs, localRegion: config.LocalRegion,
		homeRegionURLs: config.HomeRegionURLs, forwardClient: config.ForwardClient,
		trustedProxies: trustedProxies, policyConfigError: policyConfigError,
		apiKeyLeaseTTL: config.APIKeyConcurrencyLeaseTTL, apiKeyRenewInterval: config.APIKeyConcurrencyRenewInterval,
		apiKeyInflight: make(map[string]int), mux: http.NewServeMux(),
	}
	server.mux.HandleFunc("POST /v1/responses", server.createResponse)
	server.mux.HandleFunc("GET /v1/responses/{response_id}", server.getResponse)
	server.mux.HandleFunc("DELETE /v1/responses/{response_id}", server.deleteResponse)
	server.mux.HandleFunc("POST /v1/responses/{response_id}/cancel", server.cancelResponse)
	server.mux.HandleFunc("GET /v1/responses/{response_id}/input_items", server.inputItems)
	server.mux.HandleFunc("POST /v1/chat/completions", server.chatCompletions)
	server.mux.HandleFunc("POST /v1/embeddings", server.embeddings)
	server.mux.HandleFunc("POST /v1/moderations", server.moderations)
	server.mux.HandleFunc("POST /v1/rerank", server.rerank)
	server.mux.HandleFunc("GET /v1/capabilities", server.listCapabilities)
	server.mux.HandleFunc("GET /v1/models", server.listModels)
	server.mux.HandleFunc("POST /v1/conversations", server.createConversation)
	server.mux.HandleFunc("GET /v1/conversations/{conversation_id}", server.getConversation)
	server.mux.HandleFunc("DELETE /v1/conversations/{conversation_id}", server.deleteConversation)
	server.mux.HandleFunc("GET /v1/conversations/{conversation_id}/items", server.conversationItems)
	server.mux.HandleFunc("POST /v1/conversations/{conversation_id}/items", server.appendConversationItems)
	return server
}

func (s *Server) listCapabilities(responseWriter http.ResponseWriter, request *http.Request) {
	if s.capabilityCatalog == nil {
		writeError(responseWriter, http.StatusServiceUnavailable, "capability_catalog_unavailable", "capability catalog is unavailable", "")
		return
	}
	entries, err := s.capabilityCatalog.ListCapabilities(request.Context(), provider.CapabilityCatalogQuery{
		TenantID: tenantID(request), HomeRegion: s.homeRegion(request),
	})
	if err != nil {
		writeError(responseWriter, http.StatusServiceUnavailable, "capability_catalog_unavailable", err.Error(), "")
		return
	}
	data := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if err := s.authorizeModel(request, entry.ID); err != nil {
			continue
		}
		data = append(data, map[string]any{
			"id": entry.ID, "object": "capability_profile", "created": entry.Created, "capabilities": entry.Capabilities,
		})
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{"object": "list", "data": data})
}

type embeddingRequest struct {
	Model             string                 `json:"model"`
	Input             json.RawMessage        `json:"input"`
	EncodingFormat    string                 `json:"encoding_format,omitempty"`
	Dimensions        *int                   `json:"dimensions,omitempty"`
	EndUserID         string                 `json:"user,omitempty"`
	CompatibilityMode core.CompatibilityMode `json:"compatibility_mode,omitempty"`
}

func (s *Server) embeddings(responseWriter http.ResponseWriter, request *http.Request) {
	if s.capabilityRuntime == nil {
		writeError(responseWriter, http.StatusServiceUnavailable, "capability_unavailable", "embedding capability is unavailable", "")
		return
	}
	var payload embeddingRequest
	if err := decodeBody(request, &payload); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	if payload.Model == "" {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "model is required", "model")
		return
	}
	if err := s.authorizeModel(request, payload.Model); err != nil {
		writeError(responseWriter, http.StatusForbidden, "policy_denied", err.Error(), "model")
		return
	}
	if payload.EncodingFormat != "" && payload.EncodingFormat != "float" && payload.EncodingFormat != "base64" {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "encoding_format must be float or base64", "encoding_format")
		return
	}
	if payload.Dimensions != nil && (*payload.Dimensions <= 0 || *payload.Dimensions > 4096) {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "dimensions must be between 1 and 4096", "dimensions")
		return
	}
	if err := validateCompatibilityMode(payload.CompatibilityMode); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "compatibility_mode")
		return
	}
	input, err := decodeEmbeddingInput(payload.Input)
	if err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "input")
		return
	}
	tenantPolicy, apiKeyPolicy := requestPolicies(request)
	operationID, result, err := s.capabilityRuntime.Embed(request.Context(), core.EmbeddingRequest{
		CapabilityPrincipal: core.CapabilityPrincipal{
			TenantID: tenantID(request), APIKeyID: apiKeyID(request), HomeRegion: s.homeRegion(request),
			ExecutionEpoch:    s.executionEpoch(request),
			CompatibilityMode: payload.CompatibilityMode, TenantPolicy: tenantPolicy, APIKeyPolicy: apiKeyPolicy,
		},
		Model: payload.Model, Input: input, EncodingFormat: payload.EncodingFormat, Dimensions: payload.Dimensions, EndUserID: payload.EndUserID,
	})
	if operationID != "" {
		responseWriter.Header().Set("X-Gateway-Operation-ID", operationID)
	}
	if err != nil {
		writeCapabilityError(responseWriter, err)
		return
	}
	data := make([]map[string]any, 0, len(result.Data))
	for _, item := range result.Data {
		embedding := any(item.Embedding)
		if item.Base64 != "" {
			embedding = item.Base64
		}
		data = append(data, map[string]any{"object": "embedding", "index": item.Index, "embedding": embedding})
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{
		"object": "list", "data": data, "model": result.Model,
		"usage": map[string]int64{"prompt_tokens": result.InputUnits, "total_tokens": result.InputUnits},
	})
}

func decodeEmbeddingInput(raw json.RawMessage) ([]core.EmbeddingInput, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("input is required")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil, errors.New("input text cannot be empty")
		}
		return []core.EmbeddingInput{{Text: &text}}, nil
	}
	var texts []string
	if json.Unmarshal(raw, &texts) == nil {
		if len(texts) == 0 {
			return nil, errors.New("input cannot be empty")
		}
		result := make([]core.EmbeddingInput, len(texts))
		for index := range texts {
			if texts[index] == "" {
				return nil, errors.New("input text cannot be empty")
			}
			value := texts[index]
			result[index].Text = &value
		}
		return result, nil
	}
	var tokens []int64
	if json.Unmarshal(raw, &tokens) == nil && len(tokens) > 0 {
		return []core.EmbeddingInput{{Tokens: tokens}}, nil
	}
	var tokenSets [][]int64
	if json.Unmarshal(raw, &tokenSets) == nil && len(tokenSets) > 0 {
		result := make([]core.EmbeddingInput, len(tokenSets))
		for index := range tokenSets {
			if len(tokenSets[index]) == 0 {
				return nil, errors.New("token input cannot be empty")
			}
			result[index].Tokens = tokenSets[index]
		}
		return result, nil
	}
	return nil, errors.New("input must be text, text array, token array, or token-array list")
}

type moderationRequest struct {
	Model             string                 `json:"model"`
	Input             json.RawMessage        `json:"input"`
	EndUserID         string                 `json:"user,omitempty"`
	CompatibilityMode core.CompatibilityMode `json:"compatibility_mode,omitempty"`
}

func (s *Server) moderations(responseWriter http.ResponseWriter, request *http.Request) {
	if s.capabilityRuntime == nil {
		writeError(responseWriter, http.StatusServiceUnavailable, "capability_unavailable", "moderation capability is unavailable", "")
		return
	}
	var payload moderationRequest
	if err := decodeBody(request, &payload); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	if payload.Model == "" {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "model is required", "model")
		return
	}
	if err := s.authorizeModel(request, payload.Model); err != nil {
		writeError(responseWriter, http.StatusForbidden, "policy_denied", err.Error(), "model")
		return
	}
	if err := validateCompatibilityMode(payload.CompatibilityMode); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "compatibility_mode")
		return
	}
	input, err := decodeStringOrStrings(payload.Input)
	if err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "input")
		return
	}
	tenantPolicy, apiKeyPolicy := requestPolicies(request)
	operationID, result, err := s.capabilityRuntime.Moderate(request.Context(), core.ModerationRequest{
		CapabilityPrincipal: core.CapabilityPrincipal{
			TenantID: tenantID(request), APIKeyID: apiKeyID(request), HomeRegion: s.homeRegion(request),
			ExecutionEpoch:    s.executionEpoch(request),
			CompatibilityMode: payload.CompatibilityMode, TenantPolicy: tenantPolicy, APIKeyPolicy: apiKeyPolicy,
		},
		Model: payload.Model, Input: input, EndUserID: payload.EndUserID,
	})
	if operationID != "" {
		responseWriter.Header().Set("X-Gateway-Operation-ID", operationID)
	}
	if err != nil {
		writeCapabilityError(responseWriter, err)
		return
	}
	results := make([]map[string]any, len(result.Results))
	for index, item := range result.Results {
		results[index] = map[string]any{
			"flagged": item.Flagged, "categories": item.Categories, "category_scores": item.CategoryScores,
		}
		if len(item.CategoryAppliedInputTypes) > 0 {
			results[index]["category_applied_input_types"] = item.CategoryAppliedInputTypes
		}
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{"id": result.ID, "model": result.Model, "results": results})
}

func decodeStringOrStrings(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("input is required")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil, errors.New("input text cannot be empty")
		}
		return []string{text}, nil
	}
	var texts []string
	if json.Unmarshal(raw, &texts) != nil || len(texts) == 0 {
		return nil, errors.New("input must be text or a non-empty text array")
	}
	for _, value := range texts {
		if value == "" {
			return nil, errors.New("input text cannot be empty")
		}
	}
	return texts, nil
}

type rerankRequest struct {
	Model             string                 `json:"model"`
	Query             string                 `json:"query"`
	Documents         []json.RawMessage      `json:"documents"`
	TopN              *int                   `json:"top_n,omitempty"`
	ReturnDocuments   bool                   `json:"return_documents,omitempty"`
	CompatibilityMode core.CompatibilityMode `json:"compatibility_mode,omitempty"`
}

func (s *Server) rerank(responseWriter http.ResponseWriter, request *http.Request) {
	if s.capabilityRuntime == nil {
		writeError(responseWriter, http.StatusServiceUnavailable, "capability_unavailable", "rerank capability is unavailable", "")
		return
	}
	var payload rerankRequest
	if err := decodeBody(request, &payload); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	if payload.Model == "" || payload.Query == "" {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "model and query are required", "")
		return
	}
	if err := s.authorizeModel(request, payload.Model); err != nil {
		writeError(responseWriter, http.StatusForbidden, "policy_denied", err.Error(), "model")
		return
	}
	if payload.TopN != nil && (*payload.TopN <= 0 || *payload.TopN > len(payload.Documents)) {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "top_n must be positive and no greater than the document count", "top_n")
		return
	}
	if err := validateCompatibilityMode(payload.CompatibilityMode); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "compatibility_mode")
		return
	}
	documents, err := decodeRerankDocuments(payload.Documents)
	if err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "documents")
		return
	}
	tenantPolicy, apiKeyPolicy := requestPolicies(request)
	operationID, result, err := s.capabilityRuntime.Rerank(request.Context(), core.RerankRequest{
		CapabilityPrincipal: core.CapabilityPrincipal{
			TenantID: tenantID(request), APIKeyID: apiKeyID(request), HomeRegion: s.homeRegion(request),
			ExecutionEpoch:    s.executionEpoch(request),
			CompatibilityMode: payload.CompatibilityMode, TenantPolicy: tenantPolicy, APIKeyPolicy: apiKeyPolicy,
		},
		Model: payload.Model, Query: payload.Query, Documents: documents, TopN: payload.TopN, ReturnDocuments: payload.ReturnDocuments,
	})
	if operationID != "" {
		responseWriter.Header().Set("X-Gateway-Operation-ID", operationID)
	}
	if err != nil {
		writeCapabilityError(responseWriter, err)
		return
	}
	results := make([]map[string]any, len(result.Results))
	for index, item := range result.Results {
		results[index] = map[string]any{"index": item.Index, "relevance_score": item.RelevanceScore}
		if item.Document != nil {
			results[index]["document"] = map[string]string{"text": item.Document.Text}
		}
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{"id": result.ID, "model": result.Model, "results": results})
}

func decodeRerankDocuments(raw []json.RawMessage) ([]core.RerankDocument, error) {
	if len(raw) == 0 {
		return nil, errors.New("documents are required")
	}
	documents := make([]core.RerankDocument, len(raw))
	for index, encoded := range raw {
		var text string
		if json.Unmarshal(encoded, &text) != nil {
			var object struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(encoded, &object) != nil {
				return nil, fmt.Errorf("document %d must be text or an object with text", index)
			}
			text = object.Text
		}
		if text == "" {
			return nil, fmt.Errorf("document %d text cannot be empty", index)
		}
		documents[index] = core.RerankDocument{Text: text}
	}
	return documents, nil
}

func writeCapabilityError(responseWriter http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	code := "capability_error"
	if errors.Is(err, capability.ErrRouteNotFound) {
		status = http.StatusBadRequest
	} else if errors.Is(err, capability.ErrQuotaExceeded) {
		status = http.StatusTooManyRequests
		code = "rate_limit_exceeded"
	}
	writeError(responseWriter, status, code, err.Error(), "")
}

func (s *Server) listModels(responseWriter http.ResponseWriter, request *http.Request) {
	if s.modelCatalog == nil {
		writeError(responseWriter, http.StatusServiceUnavailable, "model_catalog_unavailable", "model catalog is unavailable", "")
		return
	}
	codexRequest := isCodexModelRequest(request)
	query := provider.ModelCatalogQuery{TenantID: tenantID(request), HomeRegion: s.homeRegion(request)}
	if codexRequest {
		query.RequiredFeatures = []string{"responses_native", "tools", "reasoning"}
	}
	models, err := s.modelCatalog.ListModels(request.Context(), query)
	if err != nil {
		writeError(responseWriter, http.StatusServiceUnavailable, "model_catalog_unavailable", err.Error(), "")
		return
	}
	type modelObject struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]modelObject, 0, len(models))
	for _, model := range models {
		if err := s.authorizeModel(request, model.ID); err != nil {
			continue
		}
		data = append(data, modelObject{ID: model.ID, Object: "model", Created: model.Created, OwnedBy: "gateway"})
	}
	// Codex asks the same endpoint for richer model metadata and identifies
	// itself through a query parameter or client header. Keep the default
	// response OpenAI SDK compatible while exposing the custom-provider shape.
	if codexRequest {
		codexModels := make([]map[string]any, 0, len(data))
		for index, model := range data {
			codexModels = append(codexModels, codexModelObject(model.ID, index+1))
		}
		writeJSON(responseWriter, http.StatusOK, map[string]any{"models": codexModels})
		return
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func isCodexModelRequest(request *http.Request) bool {
	if request.URL.Query().Has("client_version") {
		return true
	}
	clientMarkers := request.UserAgent() + " " + request.Header.Get("Originator") + " " + request.Header.Get("X-OpenAI-Client-User-Agent")
	return strings.Contains(strings.ToLower(clientMarkers), "codex")
}

func codexModelObject(modelID string, priority int) map[string]any {
	return map[string]any{
		"slug":                              modelID,
		"display_name":                      modelID,
		"description":                       "Model routed by LLM Gateway.",
		"model_messages":                    map[string]string{"instructions_template": "You are Codex, an agentic coding assistant. Follow developer and user instructions. Use the provided tools to inspect and modify the workspace, and continue until the task is complete."},
		"default_reasoning_level":           "low",
		"supported_reasoning_levels":        []map[string]string{{"effort": "low", "description": "Low reasoning effort"}},
		"shell_type":                        "shell_command",
		"visibility":                        "list",
		"supported_in_api":                  true,
		"priority":                          priority,
		"context_window":                    200000,
		"max_context_window":                200000,
		"effective_context_window_percent":  95,
		"input_modalities":                  []string{"text", "image"},
		"supports_search_tool":              false,
		"supports_image_detail_original":    false,
		"support_verbosity":                 false,
		"default_verbosity":                 "low",
		"default_reasoning_summary":         "none",
		"apply_patch_tool_type":             "freeform",
		"web_search_tool_type":              "text",
		"tool_mode":                         "default",
		"use_responses_lite":                false,
		"node_repl_auto_review_required":    false,
		"node_repl_disabled":                true,
		"experimental_supported_tools":      []string{},
		"additional_speed_tiers":            []string{},
		"service_tiers":                     []any{},
		"include_apps_usage_instructions":   false,
		"include_plugin_usage_instructions": false,
		"include_skills_usage_instructions": false,
		"multi_agent_version":               "v1",
		"truncation_policy":                 map[string]any{"mode": "tokens", "limit": 10000},
	}
}

func (s *Server) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		writeJSON(responseWriter, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	token, err := bearerToken(request.Header.Get("Authorization"))
	if err != nil {
		writeError(responseWriter, http.StatusUnauthorized, "authentication_error", err.Error(), "")
		return
	}
	principal, err := s.authenticator.Authenticate(request.Context(), token)
	if err != nil {
		writeError(responseWriter, http.StatusUnauthorized, "authentication_error", err.Error(), "")
		return
	}
	if principal.TenantID == "" {
		writeError(responseWriter, http.StatusUnauthorized, "authentication_error", "invalid authenticated principal", "")
		return
	}
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	if s.policyConfigError != nil {
		writeError(responseWriter, http.StatusServiceUnavailable, "policy_configuration_error", "trusted proxy policy is invalid", "")
		return
	}
	operation := operationForRequest(request)
	if err := s.authorizeRequestPolicy(request, principal, operation); err != nil {
		writeError(responseWriter, http.StatusForbidden, "policy_denied", err.Error(), "")
		return
	}
	if request.Method == http.MethodPost || request.Method == http.MethodDelete {
		homeRegion := s.homeRegion(request)
		if homeRegion != "" && homeRegion != s.localRegion {
			s.forwardToHomeRegion(responseWriter, request, homeRegion)
			return
		}
	}
	request, lease, err := s.acquireAPIKeyConcurrency(request, principal, operation)
	if err != nil {
		switch {
		case errors.Is(err, errAPIKeyConcurrencyDenied):
			writeError(responseWriter, http.StatusForbidden, "policy_denied", err.Error(), "")
		case errors.Is(err, store.ErrQuotaExceeded):
			writeError(responseWriter, http.StatusTooManyRequests, "policy_denied", "Gateway API Key concurrent Response limit exceeded", "")
		default:
			writeError(responseWriter, http.StatusServiceUnavailable, "policy_coordination_unavailable", "Gateway API Key concurrency coordination is unavailable", "")
		}
		return
	}
	defer lease.releaseAtRequestEnd()
	request = request.WithContext(context.WithValue(request.Context(), apiKeyConcurrencyLeaseContextKey{}, lease))
	s.mux.ServeHTTP(responseWriter, request)
}

func parseTrustedProxyCIDRs(values []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.String() != value {
			return nil, fmt.Errorf("invalid canonical trusted proxy CIDR %q", value)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func operationForRequest(request *http.Request) string {
	path := request.URL.Path
	switch {
	case path == "/v1/responses" || strings.HasPrefix(path, "/v1/responses/"):
		return "responses"
	case path == "/v1/chat/completions":
		return "chat_completions"
	case path == "/v1/embeddings":
		return "embeddings"
	case path == "/v1/moderations":
		return "moderation"
	case path == "/v1/rerank":
		return "rerank"
	case path == "/v1/models":
		return "models"
	case path == "/v1/capabilities":
		return "capabilities"
	case path == "/v1/conversations" || strings.HasPrefix(path, "/v1/conversations/"):
		return "conversations"
	default:
		return ""
	}
}

func (s *Server) authorizeRequestPolicy(request *http.Request, principal access.Principal, operation string) error {
	policy := principal.APIKeyPolicy
	if policy.AllowedOperations != nil && !contains(*policy.AllowedOperations, operation) {
		return fmt.Errorf("Gateway API Key does not allow operation %q", operation)
	}
	region := s.homeRegion(request)
	if policy.AllowedRegions != nil && !contains(*policy.AllowedRegions, region) {
		return fmt.Errorf("Gateway API Key does not allow region %q", region)
	}
	if policy.AllowedCIDRs != nil {
		address, ok := clientAddress(request, s.trustedProxies)
		if !ok || !addressAllowed(address, *policy.AllowedCIDRs) {
			return errors.New("Gateway API Key does not allow the trusted client address")
		}
	}
	return nil
}

func (s *Server) authorizeModel(request *http.Request, model string) error {
	allowed := authenticatedPrincipal(request).APIKeyPolicy.AllowedPublicModels
	if allowed != nil && !contains(*allowed, model) {
		return fmt.Errorf("Gateway API Key does not allow public model %q", model)
	}
	return nil
}

func contains(values []string, wanted string) bool {
	return slices.Contains(values, wanted)
}

func clientAddress(request *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	remote, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}, false
	}
	if !addressInPrefixes(remote, trusted) {
		return remote.Unmap(), true
	}
	forwarded := strings.Split(request.Header.Get("X-Forwarded-For"), ",")
	for index := len(forwarded) - 1; index >= 0; index-- {
		address, err := netip.ParseAddr(strings.TrimSpace(forwarded[index]))
		if err != nil {
			return netip.Addr{}, false
		}
		address = address.Unmap()
		if !addressInPrefixes(address, trusted) {
			return address, true
		}
		remote = address
	}
	return remote.Unmap(), true
}

func addressAllowed(address netip.Addr, values []string) bool {
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err == nil && prefix.Contains(address) {
			return true
		}
	}
	return false
}

func addressInPrefixes(address netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func (s *Server) acquireAPIKeyConcurrency(request *http.Request, principal access.Principal, operation string) (*http.Request, *apiKeyConcurrencyLease, error) {
	limit := principal.APIKeyPolicy.MaxConcurrentResponses
	if limit == nil || request.Method != http.MethodPost ||
		(operation != "responses" && operation != "chat_completions" && operation != "embeddings" && operation != "moderation" && operation != "rerank") {
		return request, &apiKeyConcurrencyLease{
			release: func() {}, backgroundContext: context.WithoutCancel(request.Context()),
		}, nil
	}
	if principal.APIKeyID == "" || *limit <= 0 {
		return nil, nil, errAPIKeyConcurrencyDenied
	}
	if quotas, ok := s.authenticator.(APIKeyConcurrencyStore); ok {
		leaseBytes := make([]byte, 16)
		if _, err := rand.Read(leaseBytes); err != nil {
			return nil, nil, fmt.Errorf("create Gateway API Key concurrency lease: %w", err)
		}
		leaseID := "key_quota_" + hex.EncodeToString(leaseBytes)
		if err := quotas.AcquireAPIKeyResponseSlot(request.Context(), principal.APIKeyID, leaseID, *limit, time.Now().UTC().Add(s.apiKeyLeaseTTL)); err != nil {
			return nil, nil, err
		}
		requestContext, cancelRequest := context.WithCancel(request.Context())
		backgroundContext, cancelBackground := context.WithCancel(context.WithoutCancel(request.Context()))
		go func() {
			ticker := time.NewTicker(s.apiKeyRenewInterval)
			defer ticker.Stop()
			for {
				select {
				case <-backgroundContext.Done():
					return
				case <-ticker.C:
					renewContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					err := quotas.RenewAPIKeyResponseSlot(renewContext, principal.APIKeyID, leaseID, time.Now().UTC().Add(s.apiKeyLeaseTTL))
					cancel()
					if err != nil {
						cancelRequest()
						cancelBackground()
						return
					}
				}
			}
		}()
		return request.WithContext(requestContext), &apiKeyConcurrencyLease{
			backgroundContext: backgroundContext,
			release: func() {
				cancelRequest()
				cancelBackground()
				releaseContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = quotas.ReleaseAPIKeyResponseSlot(releaseContext, principal.APIKeyID, leaseID)
			},
		}, nil
	}
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.apiKeyInflight[principal.APIKeyID] >= *limit {
		return nil, nil, store.ErrQuotaExceeded
	}
	s.apiKeyInflight[principal.APIKeyID]++
	return request, &apiKeyConcurrencyLease{
		backgroundContext: context.WithoutCancel(request.Context()),
		release: func() {
			s.inflightMu.Lock()
			defer s.inflightMu.Unlock()
			if s.apiKeyInflight[principal.APIKeyID] <= 1 {
				delete(s.apiKeyInflight, principal.APIKeyID)
				return
			}
			s.apiKeyInflight[principal.APIKeyID]--
		},
	}, nil
}

func bearerToken(value string) (string, error) {
	token, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || token == "" {
		return "", errors.New("missing bearer token")
	}
	return token, nil
}

func (s *Server) forwardToHomeRegion(responseWriter http.ResponseWriter, request *http.Request, homeRegion string) {
	base, err := url.Parse(s.homeRegionURLs[homeRegion])
	if err != nil || base.Scheme == "" || base.Host == "" {
		writeError(responseWriter, http.StatusMisdirectedRequest, "home_region_unavailable", "stateful write must execute in Home Region "+homeRegion, "")
		return
	}
	forwarded := request.Clone(request.Context())
	forwarded.URL.Scheme = base.Scheme
	forwarded.URL.Host = base.Host
	forwarded.URL.Path = strings.TrimRight(base.Path, "/") + request.URL.Path
	forwarded.Host = base.Host
	forwarded.RequestURI = ""
	forwarded.Header.Del("X-Authenticated-Tenant-ID")
	response, err := s.forwardClient.Do(forwarded)
	if err != nil {
		writeError(responseWriter, http.StatusBadGateway, "home_region_unavailable", err.Error(), "")
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		for _, value := range values {
			responseWriter.Header().Add(key, value)
		}
	}
	responseWriter.WriteHeader(response.StatusCode)
	destination := io.Writer(responseWriter)
	if flusher, ok := responseWriter.(http.Flusher); ok {
		destination = flushingWriter{writer: responseWriter, flusher: flusher}
	}
	_, _ = io.Copy(destination, response.Body)
}

type flushingWriter struct {
	writer  io.Writer
	flusher http.Flusher
}

func (writer flushingWriter) Write(payload []byte) (int, error) {
	written, err := writer.writer.Write(payload)
	writer.flusher.Flush()
	return written, err
}

type createResponseRequest struct {
	Model              string                      `json:"model"`
	Input              json.RawMessage             `json:"input"`
	Instructions       string                      `json:"instructions,omitempty"`
	Stream             bool                        `json:"stream"`
	Background         bool                        `json:"background"`
	Store              *bool                       `json:"store"`
	PreviousResponseID string                      `json:"previous_response_id"`
	Conversation       string                      `json:"conversation"`
	Metadata           map[string]string           `json:"metadata"`
	CompatibilityMode  core.CompatibilityMode      `json:"compatibility_mode"`
	Tools              []json.RawMessage           `json:"tools,omitempty"`
	ToolChoice         json.RawMessage             `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                       `json:"parallel_tool_calls,omitempty"`
	Reasoning          json.RawMessage             `json:"reasoning,omitempty"`
	Include            []string                    `json:"include,omitempty"`
	PromptCacheKey     string                      `json:"prompt_cache_key,omitempty"`
	ClientMetadata     json.RawMessage             `json:"client_metadata,omitempty"`
	Text               json.RawMessage             `json:"text,omitempty"`
	ServiceTier        string                      `json:"service_tier,omitempty"`
	Truncation         string                      `json:"truncation,omitempty"`
	MaxToolCalls       *int                        `json:"max_tool_calls,omitempty"`
	SafetyIdentifier   string                      `json:"safety_identifier,omitempty"`
	Temperature        *float64                    `json:"temperature,omitempty"`
	TopP               *float64                    `json:"top_p,omitempty"`
	MaxOutputTokens    *int                        `json:"max_output_tokens,omitempty"`
	Stop               json.RawMessage             `json:"stop,omitempty"`
	User               string                      `json:"user,omitempty"`
	CacheProtection    *core.CacheProtectionPolicy `json:"cache_protection,omitempty"`
}

func (s *Server) createResponse(responseWriter http.ResponseWriter, request *http.Request) {
	var payload createResponseRequest
	if err := decodeBody(request, &payload); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	if payload.Model == "" {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "model is required", "model")
		return
	}
	if err := s.authorizeModel(request, payload.Model); err != nil {
		writeError(responseWriter, http.StatusForbidden, "policy_denied", err.Error(), "model")
		return
	}
	if err := validateCompatibilityMode(payload.CompatibilityMode); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "compatibility_mode")
		return
	}
	input, err := decodeInput(payload.Input)
	if err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "input")
		return
	}
	if payload.Instructions != "" {
		input = append([]core.Item{{
			Type: "message", Role: "system",
			Content: []core.Content{{Type: "input_text", Text: payload.Instructions}},
		}}, input...)
	}
	storeResponse := true
	if payload.Store != nil {
		storeResponse = *payload.Store
	}
	if !storeResponse && payload.Conversation != "" {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "store:false cannot mutate a Conversation", "store")
		return
	}
	canonical := core.Request{
		TenantID: tenantID(request), APIKeyID: apiKeyID(request), Model: payload.Model, Input: input,
		Stream: payload.Stream, Background: payload.Background, Store: storeResponse,
		PreviousResponseID: payload.PreviousResponseID, ConversationID: payload.Conversation,
		CompatibilityMode: payload.CompatibilityMode, RequestedFeatures: requestedFeatures(input), Metadata: payload.Metadata,
		HomeRegion:        s.homeRegion(request),
		ExecutionEpoch:    s.executionEpoch(request),
		IdempotencyKey:    strings.TrimSpace(request.Header.Get("Idempotency-Key")),
		Tools:             payload.Tools,
		ToolChoice:        payload.ToolChoice,
		ParallelToolCalls: payload.ParallelToolCalls,
		Reasoning:         payload.Reasoning,
		Include:           append([]string(nil), payload.Include...),
		PromptCacheKey:    payload.PromptCacheKey,
		ClientMetadata:    payload.ClientMetadata,
		Text:              payload.Text,
		ServiceTier:       payload.ServiceTier,
		Truncation:        payload.Truncation,
		MaxToolCalls:      payload.MaxToolCalls,
		SafetyIdentifier:  payload.SafetyIdentifier,
		Temperature:       payload.Temperature,
		TopP:              payload.TopP,
		MaxOutputTokens:   payload.MaxOutputTokens,
		Stop:              payload.Stop,
		EndUserID:         payload.User,
		CacheProtection:   payload.CacheProtection,
	}
	canonical.TenantPolicy, canonical.APIKeyPolicy = requestPolicies(request)
	if len(payload.Tools) > 0 || hasJSONValue(payload.ToolChoice) {
		canonical.RequestedFeatures = append(canonical.RequestedFeatures, "tools")
	}
	if payload.Temperature != nil || payload.TopP != nil || payload.MaxOutputTokens != nil || hasJSONValue(payload.Stop) {
		canonical.RequestedFeatures = append(canonical.RequestedFeatures, "sampling")
	}
	if payload.User != "" {
		canonical.RequestedFeatures = append(canonical.RequestedFeatures, "end_user_id")
	}
	if hasJSONValue(payload.Reasoning) || slices.Contains(payload.Include, "reasoning.encrypted_content") {
		canonical.RequestedFeatures = append(canonical.RequestedFeatures, "reasoning")
	}
	if requiresNativeResponses(payload) {
		canonical.RequestedFeatures = append(canonical.RequestedFeatures, "responses_native")
	}
	if err := validateCacheProtection(payload.CacheProtection); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "cache_protection")
		return
	}
	if err := s.runtime.ValidateRequestPolicy(canonical); err != nil {
		writeRuntimeError(responseWriter, core.Response{}, err)
		return
	}
	if len(canonical.IdempotencyKey) > 256 {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "Idempotency-Key exceeds 256 bytes", "Idempotency-Key")
		return
	}
	if canonical.IdempotencyKey != "" {
		if payload.Stream || !storeResponse {
			writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "Idempotency-Key requires a stored non-streaming Response", "Idempotency-Key")
			return
		}
		canonical.RequestHash = responseRequestHash(payload)
	}
	if payload.Stream {
		canonical.RequestedFeatures = append(canonical.RequestedFeatures, "streaming")
	}
	if payload.Background && payload.Stream {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "background and stream cannot both be true", "background")
		return
	}
	if payload.Background {
		s.startBackgroundResponse(responseWriter, request, canonical)
		return
	}
	if payload.Stream {
		s.streamResponse(responseWriter, request, canonical)
		return
	}
	result, err := s.runtime.Execute(request.Context(), canonical)
	if err != nil {
		writeRuntimeError(responseWriter, result, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, result)
}

func (s *Server) startBackgroundResponse(responseWriter http.ResponseWriter, request *http.Request, canonical core.Request) {
	lease, _ := request.Context().Value(apiKeyConcurrencyLeaseContextKey{}).(*apiKeyConcurrencyLease)
	if lease != nil {
		lease.detach()
	}
	created := make(chan core.Response, 1)
	finished := make(chan struct{})
	backgroundContext := context.WithoutCancel(request.Context())
	if lease != nil && lease.backgroundContext != nil {
		backgroundContext = lease.backgroundContext
	}
	go func() {
		defer close(finished)
		if lease != nil {
			defer lease.finish()
		}
		_, _ = s.runtime.ExecuteStreaming(backgroundContext, canonical, func(event core.Event) error {
			if event.Type == "response.created" && event.Response != nil {
				select {
				case created <- *event.Response:
				default:
				}
			}
			return nil
		})
	}()
	select {
	case response := <-created:
		writeJSON(responseWriter, http.StatusOK, response)
	case <-finished:
		select {
		case response := <-created:
			writeJSON(responseWriter, http.StatusOK, response)
		default:
			writeError(responseWriter, http.StatusBadGateway, "background_start_failed", "background response failed before durable creation", "")
		}
	case <-request.Context().Done():
		writeError(responseWriter, http.StatusRequestTimeout, "request_cancelled", request.Context().Err().Error(), "")
	}
}

func (s *Server) streamResponse(responseWriter http.ResponseWriter, request *http.Request, canonical core.Request) {
	flusher, ok := responseWriter.(http.Flusher)
	if !ok {
		writeError(responseWriter, http.StatusInternalServerError, "streaming_unsupported", "response writer does not support streaming", "")
		return
	}
	responseWriter.Header().Set("Content-Type", "text/event-stream")
	responseWriter.Header().Set("Cache-Control", "no-cache, no-transform")
	responseWriter.Header().Set("Connection", "keep-alive")
	responseWriter.Header().Set("X-Accel-Buffering", "no")
	responseWriter.WriteHeader(http.StatusOK)
	flusher.Flush()

	result, err := s.runtime.ExecuteStreaming(request.Context(), canonical, func(event core.Event) error {
		if event.Type == "gateway.keepalive" {
			_, writeErr := io.WriteString(responseWriter, ": keepalive\n\n")
			flusher.Flush()
			return writeErr
		}
		return writeNamedSSE(responseWriter, event.Type, event, flusher)
	})
	if err != nil && result.Error != nil {
		_ = writeNamedSSE(responseWriter, "response.failed", core.Event{Type: "response.failed", Error: result.Error, Response: &result}, flusher)
	}
}

func (s *Server) getResponse(responseWriter http.ResponseWriter, request *http.Request) {
	result, err := s.runtime.Get(request.Context(), tenantID(request), request.PathValue("response_id"))
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, result)
}

func (s *Server) deleteResponse(responseWriter http.ResponseWriter, request *http.Request) {
	responseID := request.PathValue("response_id")
	if err := s.runtime.Delete(request.Context(), tenantID(request), responseID, s.homeRegion(request), s.executionEpoch(request)); err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{"id": responseID, "object": "response.deleted", "deleted": true})
}

func (s *Server) cancelResponse(responseWriter http.ResponseWriter, request *http.Request) {
	result, err := s.runtime.Cancel(
		request.Context(), tenantID(request), request.PathValue("response_id"), s.homeRegion(request), s.executionEpoch(request),
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeStoreError(responseWriter, err)
			return
		}
		writeError(responseWriter, http.StatusConflict, "invalid_state", err.Error(), "")
		return
	}
	writeJSON(responseWriter, http.StatusOK, result)
}

func (s *Server) inputItems(responseWriter http.ResponseWriter, request *http.Request) {
	items, err := s.runtime.InputItems(request.Context(), tenantID(request), request.PathValue("response_id"))
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{"object": "list", "data": items, "has_more": false})
}

type createConversationRequest struct {
	Items    []core.Item       `json:"items,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func (s *Server) createConversation(responseWriter http.ResponseWriter, request *http.Request) {
	var payload createConversationRequest
	if err := decodeBody(request, &payload); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	if err := validateItems(payload.Items); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "items")
		return
	}
	conversation, err := s.runtime.CreateConversation(
		request.Context(), tenantID(request), s.homeRegion(request), s.executionEpoch(request), payload.Items, payload.Metadata,
	)
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, conversation)
}

func (s *Server) getConversation(responseWriter http.ResponseWriter, request *http.Request) {
	conversation, err := s.runtime.GetConversation(request.Context(), tenantID(request), request.PathValue("conversation_id"))
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, conversation)
}

type appendConversationItemsRequest struct {
	Items            []core.Item `json:"items"`
	ExpectedRevision int64       `json:"expected_revision"`
}

func (s *Server) appendConversationItems(responseWriter http.ResponseWriter, request *http.Request) {
	var payload appendConversationItemsRequest
	if err := decodeBody(request, &payload); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	if payload.ExpectedRevision <= 0 {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "expected_revision must be positive", "expected_revision")
		return
	}
	if err := validateItems(payload.Items); err != nil || len(payload.Items) == 0 {
		if err == nil {
			err = errors.New("items cannot be empty")
		}
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "items")
		return
	}
	conversation, err := s.runtime.AppendConversationItems(
		request.Context(), tenantID(request), request.PathValue("conversation_id"), s.homeRegion(request), s.executionEpoch(request),
		payload.Items, payload.ExpectedRevision,
	)
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, conversation)
}

func (s *Server) conversationItems(responseWriter http.ResponseWriter, request *http.Request) {
	conversation, err := s.runtime.GetConversation(request.Context(), tenantID(request), request.PathValue("conversation_id"))
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{
		"object": "list", "data": conversation.Items, "has_more": false, "revision": conversation.Revision,
	})
}

func (s *Server) deleteConversation(responseWriter http.ResponseWriter, request *http.Request) {
	conversation, err := s.runtime.GetConversation(request.Context(), tenantID(request), request.PathValue("conversation_id"))
	if err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	if err := s.runtime.DeleteConversation(
		request.Context(), tenantID(request), conversation.ID, s.homeRegion(request), s.executionEpoch(request), conversation.Revision,
	); err != nil {
		writeStoreError(responseWriter, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, map[string]any{"id": conversation.ID, "object": "conversation.deleted", "deleted": true})
}

type chatCompletionRequest struct {
	Model               string                      `json:"model"`
	Messages            []chatMessage               `json:"messages"`
	Stream              bool                        `json:"stream"`
	StreamOptions       *chatStreamOptions          `json:"stream_options,omitempty"`
	Tools               []json.RawMessage           `json:"tools,omitempty"`
	ToolChoice          json.RawMessage             `json:"tool_choice,omitempty"`
	Temperature         *float64                    `json:"temperature,omitempty"`
	TopP                *float64                    `json:"top_p,omitempty"`
	MaxTokens           *int                        `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                        `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage             `json:"stop,omitempty"`
	User                string                      `json:"user,omitempty"`
	Metadata            map[string]string           `json:"metadata,omitempty"`
	CompatibilityMode   core.CompatibilityMode      `json:"compatibility_mode,omitempty"`
	CacheProtection     *core.CacheProtectionPolicy `json:"cache_protection,omitempty"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

func (s *Server) chatCompletions(responseWriter http.ResponseWriter, request *http.Request) {
	var payload chatCompletionRequest
	if err := decodeBody(request, &payload); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	if payload.Model == "" || len(payload.Messages) == 0 {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", "model and messages are required", "")
		return
	}
	if err := s.authorizeModel(request, payload.Model); err != nil {
		writeError(responseWriter, http.StatusForbidden, "policy_denied", err.Error(), "model")
		return
	}
	if err := validateCompatibilityMode(payload.CompatibilityMode); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "compatibility_mode")
		return
	}
	items, err := canonicalChatMessages(payload.Messages)
	if err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "messages")
		return
	}
	features := requestedFeatures(items)
	if payload.Stream {
		features = append(features, "streaming")
	}
	if len(payload.Tools) > 0 || hasJSONValue(payload.ToolChoice) {
		features = append(features, "tools")
	}
	if payload.Temperature != nil || payload.TopP != nil || payload.MaxTokens != nil || payload.MaxCompletionTokens != nil || hasJSONValue(payload.Stop) {
		features = append(features, "sampling")
	}
	if payload.User != "" {
		features = append(features, "end_user_id")
	}
	maxOutputTokens := payload.MaxCompletionTokens
	if maxOutputTokens == nil {
		maxOutputTokens = payload.MaxTokens
	}
	canonical := core.Request{
		TenantID: tenantID(request), APIKeyID: apiKeyID(request), Model: payload.Model, Input: items, Stream: payload.Stream, Store: true,
		CompatibilityMode: payload.CompatibilityMode, RequestedFeatures: features, Metadata: payload.Metadata,
		HomeRegion: s.homeRegion(request), ExecutionEpoch: s.executionEpoch(request),
		Tools: payload.Tools, ToolChoice: payload.ToolChoice, Temperature: payload.Temperature,
		TopP: payload.TopP, MaxOutputTokens: maxOutputTokens, Stop: payload.Stop, EndUserID: payload.User,
		CacheProtection: payload.CacheProtection,
	}
	canonical.TenantPolicy, canonical.APIKeyPolicy = requestPolicies(request)
	if err := validateCacheProtection(payload.CacheProtection); err != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid_request_error", err.Error(), "cache_protection")
		return
	}
	if err := s.runtime.ValidateRequestPolicy(canonical); err != nil {
		writeRuntimeError(responseWriter, core.Response{}, err)
		return
	}
	if payload.Stream {
		s.streamChatCompletion(responseWriter, request, canonical, payload.StreamOptions)
		return
	}
	result, err := s.runtime.Execute(request.Context(), canonical)
	if err != nil {
		writeRuntimeError(responseWriter, result, err)
		return
	}
	writeJSON(responseWriter, http.StatusOK, chatCompletion(result))
}

func (s *Server) streamChatCompletion(responseWriter http.ResponseWriter, request *http.Request, canonical core.Request, options *chatStreamOptions) {
	flusher, ok := responseWriter.(http.Flusher)
	if !ok {
		writeError(responseWriter, http.StatusInternalServerError, "streaming_unsupported", "response writer does not support streaming", "")
		return
	}
	responseWriter.Header().Set("Content-Type", "text/event-stream")
	responseWriter.Header().Set("Cache-Control", "no-cache, no-transform")
	responseWriter.Header().Set("Connection", "keep-alive")
	responseWriter.Header().Set("X-Accel-Buffering", "no")
	responseWriter.WriteHeader(http.StatusOK)
	flusher.Flush()

	var responseID string
	var createdAt int64
	toolCallOutput := false
	toolCallIndex := 0
	emit := func(event core.Event) error {
		if event.Type == "gateway.keepalive" {
			_, err := io.WriteString(responseWriter, ": keepalive\n\n")
			flusher.Flush()
			return err
		}
		if event.Response != nil && responseID == "" {
			responseID = event.Response.ID
			createdAt = event.Response.CreatedAt
		}
		switch event.Type {
		case "response.created":
			return writeSSE(responseWriter, map[string]any{
				"id": responseID, "object": "chat.completion.chunk", "created": createdAt, "model": canonical.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
			}, flusher)
		case "response.output_text.delta":
			return writeSSE(responseWriter, map[string]any{
				"id": responseID, "object": "chat.completion.chunk", "created": createdAt, "model": canonical.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": event.Delta}, "finish_reason": nil}},
			}, flusher)
		case "response.output_item.done":
			if event.Item == nil || event.Item.Type != "function_call" {
				return nil
			}
			toolCallOutput = true
			index := toolCallIndex
			toolCallIndex++
			return writeSSE(responseWriter, map[string]any{
				"id": responseID, "object": "chat.completion.chunk", "created": createdAt, "model": canonical.Model,
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": index, "id": event.Item.CallID, "type": "function",
						"function": map[string]any{"name": event.Item.Name, "arguments": string(event.Item.Arguments)},
					}}},
					"finish_reason": nil,
				}},
			}, flusher)
		case "response.completed":
			finishReason := "stop"
			if toolCallOutput {
				finishReason = "tool_calls"
			}
			chunk := map[string]any{
				"id": responseID, "object": "chat.completion.chunk", "created": createdAt, "model": canonical.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
			}
			if options != nil && options.IncludeUsage && event.Usage != nil {
				chunk["usage"] = chatUsage(*event.Usage)
			}
			return writeSSE(responseWriter, chunk, flusher)
		}
		return nil
	}
	result, err := s.runtime.ExecuteStreaming(request.Context(), canonical, emit)
	if err != nil && result.Error != nil {
		_ = writeSSE(responseWriter, map[string]any{"error": result.Error}, flusher)
	}
	_, _ = io.WriteString(responseWriter, "data: [DONE]\n\n")
	flusher.Flush()
}

func canonicalChatMessages(messages []chatMessage) ([]core.Item, error) {
	items := make([]core.Item, 0, len(messages))
	for _, message := range messages {
		if message.Role == "" {
			return nil, errors.New("message role is required")
		}
		var text string
		contentPresent := hasJSONValue(message.Content)
		var content []core.Content
		if contentPresent && json.Unmarshal(message.Content, &text) != nil {
			var parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text,omitempty"`
				ImageURL *struct {
					URL    string `json:"url"`
					Detail string `json:"detail,omitempty"`
				} `json:"image_url,omitempty"`
			}
			if err := json.Unmarshal(message.Content, &parts); err != nil || len(parts) == 0 {
				return nil, errors.New("chat message content must be a string or a non-empty content-part array")
			}
			for _, part := range parts {
				switch part.Type {
				case "text":
					if part.Text == "" {
						return nil, errors.New("text content parts require text")
					}
					content = append(content, core.Content{Type: "input_text", Text: part.Text})
				case "image_url":
					if part.ImageURL == nil || part.ImageURL.URL == "" {
						return nil, errors.New("image_url content parts require a URL")
					}
					content = append(content, core.Content{Type: "input_image", ImageURL: part.ImageURL.URL, Detail: part.ImageURL.Detail})
				default:
					return nil, fmt.Errorf("unsupported chat content part type %q", part.Type)
				}
			}
		}
		item := core.Item{Type: "message", Role: message.Role, Name: message.Name, CallID: message.ToolCallID}
		if message.Role == "tool" {
			if !contentPresent || message.ToolCallID == "" {
				return nil, errors.New("tool messages require string content and tool_call_id")
			}
			item.Type = "function_call_output"
			item.Output = text
			items = append(items, item)
		} else if contentPresent {
			if content == nil {
				content = []core.Content{{Type: "input_text", Text: text}}
			}
			item.Content = content
			items = append(items, item)
		} else if len(message.ToolCalls) == 0 {
			return nil, errors.New("chat messages require string content unless assistant tool_calls are present")
		}
		for _, call := range message.ToolCalls {
			arguments := json.RawMessage(call.Function.Arguments)
			if message.Role != "assistant" || call.Type != "function" || call.ID == "" || call.Function.Name == "" || !json.Valid(arguments) {
				return nil, errors.New("assistant tool_calls require id, function name, and JSON arguments")
			}
			items = append(items, core.Item{
				Type: "function_call", CallID: call.ID, Name: call.Function.Name, Arguments: arguments,
			})
		}
	}
	return items, nil
}

func chatCompletion(response core.Response) map[string]any {
	message := map[string]any{"role": "assistant", "content": response.OutputText()}
	finishReason := "stop"
	var toolCalls []any
	for _, item := range response.Output {
		if item.Type != "function_call" {
			continue
		}
		toolCalls = append(toolCalls, map[string]any{
			"id": item.CallID, "type": "function",
			"function": map[string]any{"name": item.Name, "arguments": string(item.Arguments)},
		})
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
		if response.OutputText() == "" {
			message["content"] = nil
		}
	}
	return map[string]any{
		"id": response.ID, "object": "chat.completion", "created": response.CreatedAt, "model": response.Model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finishReason,
		}},
		"usage": chatUsage(response.Usage),
	}
}

func hasJSONValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func chatUsage(usage core.Usage) map[string]any {
	return map[string]any{
		"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens, "total_tokens": usage.TotalTokens,
		"prompt_tokens_details": map[string]int64{"cached_tokens": usage.CachedInputTokens},
	}
}

func writeSSE(responseWriter http.ResponseWriter, value any, flusher http.Flusher) error {
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(value); err != nil {
		return err
	}
	data := strings.TrimSuffix(buffer.String(), "\n")
	if _, err := fmt.Fprintf(responseWriter, "data: %s\n\n", data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeNamedSSE(responseWriter http.ResponseWriter, eventName string, value any, flusher http.Flusher) error {
	var buffer bytes.Buffer
	if err := json.NewEncoder(&buffer).Encode(value); err != nil {
		return err
	}
	data := strings.TrimSuffix(buffer.String(), "\n")
	if _, err := fmt.Fprintf(responseWriter, "event: %s\ndata: %s\n\n", eventName, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeRuntimeError(responseWriter http.ResponseWriter, result core.Response, err error) {
	if errors.Is(err, runtime.ErrQuotaExceeded) {
		writeError(responseWriter, http.StatusTooManyRequests, "rate_limit_exceeded", err.Error(), "")
		return
	}
	if errors.Is(err, runtime.ErrCacheProtectionNotAllowed) {
		writeError(responseWriter, http.StatusForbidden, "cache_protection_not_allowed", err.Error(), "cache_protection")
		return
	}
	if errors.Is(err, store.ErrIdempotencyMismatch) {
		writeError(responseWriter, http.StatusConflict, "idempotency_conflict", store.ErrIdempotencyMismatch.Error(), "Idempotency-Key")
		return
	}
	status := http.StatusBadGateway
	if result.Error != nil && result.Error.Code == "route_not_found" {
		status = http.StatusBadRequest
	}
	if result.Error != nil {
		writeError(responseWriter, status, result.Error.Code, result.Error.Message, "")
		return
	}
	writeError(responseWriter, status, "gateway_error", err.Error(), "")
}

func responseRequestHash(payload createResponseRequest) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return digest[:]
}

func decodeInput(raw json.RawMessage) ([]core.Item, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("input is required")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []core.Item{{Type: "message", Role: "user", Content: []core.Content{{Type: "input_text", Text: text}}}}, nil
	}
	var items []core.Item
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("input must be a string or an array of typed items")
	}
	if err := validateItems(items); err != nil {
		return nil, err
	}
	return items, nil
}

func validateItems(items []core.Item) error {
	for index := range items {
		if items[index].Type == "" {
			return errors.New("every item must have a type")
		}
		for _, content := range items[index].Content {
			switch content.Type {
			case "input_text", "output_text", "text":
				if content.Text == "" {
					return errors.New("text content requires text")
				}
			case "input_image":
				if content.ImageURL == "" && content.FileID == "" {
					return errors.New("input_image content requires image_url or file_id")
				}
			case "input_file":
				if content.FileData != "" {
					return errors.New("inline file_data is not accepted; upload to regional object storage and use an immutable file_id")
				}
				if content.FileID == "" {
					return errors.New("input_file content requires an immutable file_id")
				}
			default:
				return fmt.Errorf("unsupported content type %q", content.Type)
			}
		}
	}
	return nil
}

func requestedFeatures(items []core.Item) []string {
	features := []string{"text"}
	multimodal, files, reasoning := false, false, false
	for _, item := range items {
		if item.Type == "reasoning" || len(item.Summary) > 0 || item.EncryptedContent != "" || len(item.ProviderMetadata) > 0 {
			reasoning = true
		}
		for _, content := range item.Content {
			if content.Type == "input_image" {
				multimodal = true
			} else if content.Type == "input_file" {
				files = true
			}
		}
	}
	if multimodal {
		features = append(features, "multimodal")
	}
	if files {
		features = append(features, "files")
	}
	if reasoning {
		features = append(features, "reasoning")
	}
	return features
}

func requiresNativeResponses(payload createResponseRequest) bool {
	if payload.ParallelToolCalls != nil || hasJSONValue(payload.Reasoning) || len(payload.Include) > 0 ||
		payload.PromptCacheKey != "" || hasJSONValue(payload.ClientMetadata) || hasJSONValue(payload.Text) ||
		payload.ServiceTier != "" || payload.Truncation != "" || payload.MaxToolCalls != nil || payload.SafetyIdentifier != "" {
		return true
	}
	for _, raw := range payload.Tools {
		var tool struct {
			Type     string          `json:"type"`
			Name     string          `json:"name"`
			Function json.RawMessage `json:"function"`
		}
		if json.Unmarshal(raw, &tool) != nil || tool.Type != "function" || tool.Name != "" || len(tool.Function) == 0 {
			return true
		}
	}
	return false
}

func validateCacheProtection(policy *core.CacheProtectionPolicy) error {
	if policy == nil || !policy.Enabled {
		return nil
	}
	if policy.MaxSpendMicros <= 0 || policy.MaxRefreshes <= 0 || policy.MaxProtectionWindowSec <= 0 {
		return errors.New("enabled cache protection requires positive max_spend_micros, max_refreshes, and max_protection_window_seconds")
	}
	if policy.SafetyMarginMicros < 0 {
		return errors.New("cache protection safety_margin_micros cannot be negative")
	}
	if policy.MaxSpendMicros > 1_000_000_000 || policy.MaxRefreshes > 100 || policy.MaxProtectionWindowSec > int64((24*time.Hour)/time.Second) {
		return errors.New("cache protection bounds exceed the supported safety limits")
	}
	return nil
}

func validateCompatibilityMode(mode core.CompatibilityMode) error {
	if mode == "" || mode == core.CompatibilityStrict || mode == core.CompatibilityBestEffort {
		return nil
	}
	return errors.New("compatibility_mode must be strict or best_effort")
}

func decodeBody(request *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func authenticatedPrincipal(request *http.Request) access.Principal {
	principal, _ := request.Context().Value(principalContextKey{}).(access.Principal)
	return principal
}

func tenantID(request *http.Request) string { return authenticatedPrincipal(request).TenantID }

func apiKeyID(request *http.Request) string { return authenticatedPrincipal(request).APIKeyID }

func requestPolicies(request *http.Request) (*core.TenantPolicy, *core.APIKeyPolicy) {
	principal := authenticatedPrincipal(request)
	var tenantPolicy *core.TenantPolicy
	var apiKeyPolicy *core.APIKeyPolicy
	if principal.TenantPolicy.Revision > 0 {
		policy := principal.TenantPolicy
		tenantPolicy = &policy
	}
	if principal.APIKeyPolicy.Revision > 0 {
		policy := principal.APIKeyPolicy
		apiKeyPolicy = &policy
	}
	return tenantPolicy, apiKeyPolicy
}

func (s *Server) homeRegion(request *http.Request) string {
	if region := authenticatedPrincipal(request).HomeRegion; region != "" {
		return region
	}
	if region := s.homeRegions[tenantID(request)]; region != "" {
		return region
	}
	return "local"
}

func (s *Server) executionEpoch(request *http.Request) int64 {
	if epoch := authenticatedPrincipal(request).ExecutionEpoch; epoch > 0 {
		return epoch
	}
	if epoch := s.executionEpochs[tenantID(request)]; epoch > 0 {
		return epoch
	}
	return 1
}

func writeStoreError(responseWriter http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(responseWriter, http.StatusNotFound, "not_found", "response not found", "")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(responseWriter, http.StatusConflict, "conflict", err.Error(), "")
		return
	}
	if errors.Is(err, store.ErrConversationBusy) {
		writeError(responseWriter, http.StatusConflict, "conversation_busy", err.Error(), "conversation")
		return
	}
	writeError(responseWriter, http.StatusInternalServerError, "store_error", err.Error(), "")
}

func writeError(responseWriter http.ResponseWriter, status int, code, message, param string) {
	writeJSON(responseWriter, status, map[string]any{"error": core.Error{Code: code, Message: message, Type: "invalid_request_error", Param: param}})
}

func writeJSON(responseWriter http.ResponseWriter, status int, value any) {
	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.WriteHeader(status)
	_ = json.NewEncoder(responseWriter).Encode(value)
}

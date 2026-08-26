package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/core"
)

type EmbeddingExecutor interface {
	Embed(context.Context, core.EmbeddingRequest) (core.EmbeddingResult, error)
}

type ModerationExecutor interface {
	Moderate(context.Context, core.ModerationRequest) (core.ModerationResult, error)
}

type RerankExecutor interface {
	Rerank(context.Context, core.RerankRequest) (core.RerankResult, error)
}

type ExecutionError struct {
	err                error
	sideEffectPossible bool
}

func NewExecutionError(err error, sideEffectPossible bool) error {
	if err == nil {
		return nil
	}
	return &ExecutionError{err: err, sideEffectPossible: sideEffectPossible}
}

func (e *ExecutionError) Error() string { return e.err.Error() }
func (e *ExecutionError) Unwrap() error { return e.err }

func SideEffectPossible(err error) bool {
	var executionError *ExecutionError
	return errors.As(err, &executionError) && executionError.sideEffectPossible
}

type CapabilityRouteQuery struct {
	TenantID          string
	Model             string
	HomeRegion        string
	Capability        core.Capability
	CompatibilityMode core.CompatibilityMode
}

type CapabilityRouter interface {
	CapabilityCandidates(context.Context, CapabilityRouteQuery) ([]Route, error)
}

type CapabilityCatalogQuery struct {
	TenantID   string
	HomeRegion string
}

type CapabilityCatalogEntry struct {
	ID           string
	Created      int64
	Capabilities map[string]CapabilitySupport
}

type CapabilityCatalog interface {
	ListCapabilities(context.Context, CapabilityCatalogQuery) ([]CapabilityCatalogEntry, error)
}

func (r *StaticRouter) ListCapabilities(_ context.Context, query CapabilityCatalogQuery) ([]CapabilityCatalogEntry, error) {
	snapshot := r.snapshot.Load()
	if snapshot == nil {
		return nil, errors.New("model route configuration is unavailable")
	}
	models := make(map[string]map[string]CapabilitySupport)
	for _, route := range snapshot.routes {
		if !route.Healthy || route.Model == "" ||
			(query.HomeRegion != "" && route.HomeRegion != "" && query.HomeRegion != route.HomeRegion) {
			continue
		}
		if !routeVisibleToTenant(route, query.TenantID) {
			continue
		}
		for _, capability := range []core.Capability{core.CapabilityEmbeddings, core.CapabilityModeration, core.CapabilityRerank} {
			support := route.Profile.Features[string(capability)]
			if support != CapabilityNative && support != CapabilityTranslated {
				continue
			}
			if models[route.Model] == nil {
				models[route.Model] = make(map[string]CapabilitySupport)
			}
			current := models[route.Model][string(capability)]
			if current == "" || support == CapabilityNative {
				models[route.Model][string(capability)] = support
			}
		}
	}
	entries := make([]CapabilityCatalogEntry, 0, len(models))
	for model, capabilities := range models {
		entries = append(entries, CapabilityCatalogEntry{ID: model, Created: snapshot.created, Capabilities: capabilities})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, nil
}

func (r *StaticRouter) CapabilityCandidates(_ context.Context, query CapabilityRouteQuery) ([]Route, error) {
	snapshot := r.snapshot.Load()
	if snapshot == nil {
		return nil, errors.New("model route configuration is unavailable")
	}
	var candidates []Route
	for _, route := range snapshot.routes {
		if !route.Healthy || (query.Model != route.Model && query.Model != route.ID) {
			continue
		}
		if !routeVisibleToTenant(route, query.TenantID) {
			continue
		}
		if query.HomeRegion != "" && route.HomeRegion != "" && query.HomeRegion != route.HomeRegion {
			continue
		}
		support := route.Profile.Features[string(query.Capability)]
		if support == "" || support == CapabilityUnsupported ||
			(query.CompatibilityMode != core.CompatibilityBestEffort && support != CapabilityNative) {
			continue
		}
		candidates = append(candidates, route)
	}
	if len(candidates) == 0 {
		return nil, errors.New("no compatible model route")
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].InputCost+candidates[i].OutputCost < candidates[j].InputCost+candidates[j].OutputCost
	})
	return candidates, nil
}

type DeterministicCapabilityExecutor struct{}

func NewDeterministicCapabilityExecutor() DeterministicCapabilityExecutor {
	return DeterministicCapabilityExecutor{}
}

func (DeterministicCapabilityExecutor) Embed(_ context.Context, request core.EmbeddingRequest) (core.EmbeddingResult, error) {
	dimensions := 8
	if request.Dimensions != nil {
		dimensions = *request.Dimensions
	}
	if dimensions <= 0 || dimensions > 4096 {
		return core.EmbeddingResult{}, errors.New("dimensions must be between 1 and 4096")
	}
	data := make([]core.EmbeddingData, 0, len(request.Input))
	for index, input := range request.Input {
		payload, err := json.Marshal(input)
		if err != nil {
			return core.EmbeddingResult{}, err
		}
		digest := sha256.Sum256(payload)
		vector := make([]float64, dimensions)
		for dimension := range vector {
			offset := (dimension * 4) % len(digest)
			bits := binary.BigEndian.Uint32(digest[offset : offset+4])
			vector[dimension] = (float64(bits)/float64(math.MaxUint32))*2 - 1
		}
		item := core.EmbeddingData{Index: index, Embedding: vector}
		if request.EncodingFormat == "base64" {
			encoded := make([]byte, len(vector)*4)
			for dimension, value := range vector {
				binary.LittleEndian.PutUint32(encoded[dimension*4:], math.Float32bits(float32(value)))
			}
			item.Embedding = nil
			item.Base64 = base64.StdEncoding.EncodeToString(encoded)
		}
		data = append(data, item)
	}
	usage, _ := json.Marshal(map[string]any{"input_units": len(request.Input), "synthetic": true})
	return core.EmbeddingResult{
		Model: request.Model, Data: data, InputUnits: int64(len(request.Input)), Dimensions: int64(dimensions), ProviderUsage: usage,
	}, nil
}

func (DeterministicCapabilityExecutor) Moderate(_ context.Context, request core.ModerationRequest) (core.ModerationResult, error) {
	results := make([]core.ModerationResultItem, len(request.Input))
	for index, input := range request.Input {
		flagged := containsUnsafe(input)
		score := 0.0
		if flagged {
			score = 1
		}
		results[index] = core.ModerationResultItem{
			Flagged: flagged, Categories: map[string]bool{"unsafe": flagged}, CategoryScores: map[string]float64{"unsafe": score},
		}
	}
	usage, _ := json.Marshal(map[string]any{"input_units": len(request.Input), "synthetic": true})
	return core.ModerationResult{Model: request.Model, Results: results, InputUnits: int64(len(request.Input)), ProviderUsage: usage}, nil
}

func containsUnsafe(value string) bool {
	for index := 0; index+6 <= len(value); index++ {
		candidate := value[index : index+6]
		if candidate == "unsafe" || candidate == "UNSAFE" || candidate == "Unsafe" {
			return true
		}
	}
	return false
}

func (DeterministicCapabilityExecutor) Rerank(_ context.Context, request core.RerankRequest) (core.RerankResult, error) {
	queryTerms := make(map[string]struct{})
	for _, term := range strings.Fields(strings.ToLower(request.Query)) {
		queryTerms[term] = struct{}{}
	}
	results := make([]core.RerankResultItem, len(request.Documents))
	for index, document := range request.Documents {
		matches := 0
		seen := make(map[string]struct{})
		for _, term := range strings.Fields(strings.ToLower(document.Text)) {
			if _, wanted := queryTerms[term]; !wanted {
				continue
			}
			if _, duplicate := seen[term]; duplicate {
				continue
			}
			seen[term] = struct{}{}
			matches++
		}
		score := 0.0
		if len(queryTerms) > 0 {
			score = float64(matches) / float64(len(queryTerms))
		}
		result := core.RerankResultItem{Index: index, RelevanceScore: score}
		if request.ReturnDocuments {
			copy := document
			result.Document = &copy
		}
		results[index] = result
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].RelevanceScore == results[j].RelevanceScore {
			return results[i].Index < results[j].Index
		}
		return results[i].RelevanceScore > results[j].RelevanceScore
	})
	if request.TopN != nil && *request.TopN < len(results) {
		results = results[:*request.TopN]
	}
	providerTokens := int64(len([]byte(request.Query)))
	for _, document := range request.Documents {
		providerTokens += int64(len([]byte(document.Text)))
	}
	usage, _ := json.Marshal(map[string]any{
		"documents": len(request.Documents), "provider_tokens": providerTokens, "synthetic": true,
	})
	return core.RerankResult{
		Model: request.Model, Results: results, Documents: int64(len(request.Documents)), ProviderTokens: providerTokens, ProviderUsage: usage,
	}, nil
}

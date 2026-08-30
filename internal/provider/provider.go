package provider

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
)

type EventStream interface {
	Recv() (core.Event, error)
	Close() error
}

type ResponseExecutor interface {
	Execute(context.Context, core.Request) (EventStream, error)
}

type CacheAnchor struct {
	TenantID         string
	APIKeyID         string
	RouteID          string
	Provider         string
	Model            string
	CredentialScope  string
	Region           string
	CacheKey         string
	PrefixHash       string
	SerializedPrefix json.RawMessage
}

type CacheCapability struct {
	Supported bool
	Reason    string
}

type RefreshResult struct {
	Status        string
	Usage         core.Usage
	UsageReliable bool
	ProviderUsage json.RawMessage
	ExpiresAt     time.Time
}

type CacheProtector interface {
	Inspect(context.Context, CacheAnchor) CacheCapability
	Refresh(context.Context, CacheAnchor) (RefreshResult, error)
}

type CacheObservation struct {
	Anchor             CacheAnchor
	EstimatedExpiresAt time.Time
	PrefixTokens       int64
	RefreshCostMicros  int64
	SideEffecting      bool
}

type CacheAnchorBuilder interface {
	CurrentCacheAnchor(context.Context, core.Request) (CacheAnchor, bool, error)
	BuildCacheAnchor(context.Context, core.Request, core.Response) (CacheObservation, error)
}

type CapabilitySupport string

const (
	CapabilityNative      CapabilitySupport = "native"
	CapabilityTranslated  CapabilitySupport = "translated"
	CapabilityUnsupported CapabilitySupport = "unsupported"
)

type CapabilityProfile struct {
	Revision int64                        `json:"revision"`
	Features map[string]CapabilitySupport `json:"features"`
}

type RouteAdministrativeStatus string

const (
	RouteActive   RouteAdministrativeStatus = "active"
	RouteDraining RouteAdministrativeStatus = "draining"
	RouteDisabled RouteAdministrativeStatus = "disabled"
)

type Route struct {
	ID                   string
	Provider             string
	Model                string
	Region               string
	CredentialScope      string
	HomeRegion           string
	TenantIDs            []string
	Healthy              bool
	InputCost            float64
	OutputCost           float64
	Profile              CapabilityProfile
	Executor             ResponseExecutor
	EmbeddingExecutor    EmbeddingExecutor
	ModerationExecutor   ModerationExecutor
	RerankExecutor       RerankExecutor
	CacheProtector       CacheProtector
	CacheAnchorBuilder   CacheAnchorBuilder
	PriceSnapshot        core.PriceSnapshot
	CacheUsageReliable   bool
	AdministrativeStatus RouteAdministrativeStatus
	Priority             int
	Weight               int
	MaxConcurrency       int
	StickyRouting        bool
}

type Router interface {
	Candidates(context.Context, core.Request) ([]Route, error)
}

type ModelCatalogQuery struct {
	TenantID         string
	HomeRegion       string
	RequiredFeatures []string
}

type ModelCatalogEntry struct {
	ID      string
	Created int64
}

type ModelCatalog interface {
	ListModels(context.Context, ModelCatalogQuery) ([]ModelCatalogEntry, error)
}

type StaticRouter struct {
	snapshot atomic.Pointer[routeSnapshot]
}

type routeSnapshot struct {
	revision int64
	created  int64
	routes   []Route
}

func NewStaticRouter(executor ResponseExecutor) *StaticRouter {
	return NewStaticRouterForTenants(executor, nil)
}

func NewStaticRouterForTenants(executor ResponseExecutor, tenantIDs []string) *StaticRouter {
	route := Route{
		ID: "echo-default", Provider: "echo", Model: "echo-v1", Region: "local",
		HomeRegion: "local", TenantIDs: append([]string(nil), tenantIDs...), Healthy: true, Executor: executor,
		Profile: CapabilityProfile{Revision: 1, Features: map[string]CapabilitySupport{
			"text": CapabilityNative, "streaming": CapabilityNative,
		}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "price_echo_v1", Provider: "echo", Model: "echo-v1", Region: "local", Currency: "USD",
			EffectiveAt: 0, Source: "builtin-zero-cost",
		},
		CacheUsageReliable: true,
	}
	if _, ok := executor.(EchoExecutor); ok {
		deterministic := NewDeterministicCapabilityExecutor()
		route.EmbeddingExecutor = deterministic
		route.ModerationExecutor = deterministic
		route.RerankExecutor = deterministic
		route.Profile.Features["embeddings"] = CapabilityNative
		route.Profile.Features["moderation"] = CapabilityNative
		route.Profile.Features["rerank"] = CapabilityNative
	}
	return NewVersionedRouter(1, []Route{route})
}

func NewRouter(routes ...Route) *StaticRouter {
	return NewVersionedRouter(1, routes)
}

func NewVersionedRouter(revision int64, routes []Route) *StaticRouter {
	return NewVersionedRouterAt(revision, time.Now(), routes)
}

func NewVersionedRouterAt(revision int64, createdAt time.Time, routes []Route) *StaticRouter {
	if revision <= 0 {
		panic("route snapshot revision must be positive")
	}
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	router := &StaticRouter{}
	router.snapshot.Store(&routeSnapshot{revision: revision, created: createdAt.Unix(), routes: append([]Route(nil), routes...)})
	return router
}

func (r *StaticRouter) Update(revision int64, routes []Route) error {
	return r.UpdateAt(revision, time.Now(), routes)
}

func (r *StaticRouter) UpdateAt(revision int64, createdAt time.Time, routes []Route) error {
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	for {
		current := r.snapshot.Load()
		if current != nil && revision <= current.revision {
			return errors.New("route snapshot revision must increase")
		}
		next := &routeSnapshot{revision: revision, created: createdAt.Unix(), routes: append([]Route(nil), routes...)}
		if r.snapshot.CompareAndSwap(current, next) {
			return nil
		}
	}
}

// ReplaceAt installs a durable published snapshot. It permits replacing an
// equal-revision legacy bootstrap snapshot when the managed projection is
// applied for the first time, but never permits revision rollback.
func (r *StaticRouter) ReplaceAt(revision int64, createdAt time.Time, routes []Route) error {
	if revision <= 0 {
		return errors.New("route snapshot revision must be positive")
	}
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	for {
		current := r.snapshot.Load()
		if current != nil && revision < current.revision {
			return errors.New("route snapshot revision must not decrease")
		}
		next := &routeSnapshot{revision: revision, created: createdAt.Unix(), routes: append([]Route(nil), routes...)}
		if r.snapshot.CompareAndSwap(current, next) {
			return nil
		}
	}
}

func (r *StaticRouter) Revision() int64 {
	if snapshot := r.snapshot.Load(); snapshot != nil {
		return snapshot.revision
	}
	return 0
}

func (r *StaticRouter) ListModels(_ context.Context, query ModelCatalogQuery) ([]ModelCatalogEntry, error) {
	snapshot := r.snapshot.Load()
	if snapshot == nil {
		return nil, errors.New("model route configuration is unavailable")
	}
	models := make(map[string]struct{})
	for _, route := range snapshot.routes {
		if !route.Healthy || !routeAcceptsNewAssignments(route) || route.Model == "" || route.Profile.Features["text"] != CapabilityNative {
			continue
		}
		if !routeVisibleToTenant(route, query.TenantID) {
			continue
		}
		compatible := true
		for _, feature := range query.RequiredFeatures {
			if route.Profile.Features[feature] != CapabilityNative {
				compatible = false
				break
			}
		}
		if !compatible {
			continue
		}
		if query.HomeRegion != "" && route.HomeRegion != "" && route.HomeRegion != query.HomeRegion {
			continue
		}
		models[route.Model] = struct{}{}
	}
	entries := make([]ModelCatalogEntry, 0, len(models))
	for model := range models {
		entries = append(entries, ModelCatalogEntry{ID: model, Created: snapshot.created})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	return entries, nil
}

func (r *StaticRouter) ResolveCacheProtector(anchor CacheAnchor) CacheProtector {
	snapshot := r.snapshot.Load()
	if snapshot == nil {
		return nil
	}
	for _, route := range snapshot.routes {
		if route.ID == anchor.RouteID && route.Provider == anchor.Provider && route.Region == anchor.Region &&
			route.CredentialScope == anchor.CredentialScope && route.CacheProtector != nil {
			return route.CacheProtector
		}
	}
	return nil
}

func (r *StaticRouter) Candidates(_ context.Context, request core.Request) ([]Route, error) {
	snapshot := r.snapshot.Load()
	if snapshot == nil {
		return nil, errors.New("model route configuration is unavailable")
	}
	var candidates []Route
	for _, route := range snapshot.routes {
		if !route.Healthy || !routeAcceptsRequest(route, request.PreferredRouteID) || (request.Model != route.Model && request.Model != route.ID) {
			continue
		}
		if !routeVisibleToTenant(route, request.TenantID) {
			continue
		}
		if request.HomeRegion != "" && route.HomeRegion != "" && route.HomeRegion != request.HomeRegion {
			continue
		}
		compatible := true
		for _, feature := range request.RequestedFeatures {
			support := route.Profile.Features[feature]
			if support == CapabilityUnsupported || support == "" || (request.CompatibilityMode == core.CompatibilityStrict && support != CapabilityNative) {
				compatible = false
				break
			}
		}
		if compatible {
			candidates = append(candidates, route)
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("no compatible model route")
	}
	weighted := false
	for _, candidate := range candidates {
		if candidate.Weight > 0 {
			weighted = true
			break
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if request.PreferredRouteID != "" {
			iPreferred := candidates[i].ID == request.PreferredRouteID
			jPreferred := candidates[j].ID == request.PreferredRouteID
			if iPreferred != jPreferred {
				return iPreferred
			}
		}
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		if weighted {
			iScore := weightedRouteScore(request, candidates[i])
			jScore := weightedRouteScore(request, candidates[j])
			if iScore != jScore {
				return iScore < jScore
			}
		}
		iCost := candidates[i].InputCost + candidates[i].OutputCost
		jCost := candidates[j].InputCost + candidates[j].OutputCost
		return iCost < jCost
	})
	return candidates, nil
}

func weightedRouteScore(request core.Request, route Route) float64 {
	weight := route.Weight
	if weight <= 0 {
		weight = 1
	}
	identity := ""
	if route.StickyRouting && request.ExperimentIdentity != "" {
		identity = request.ExperimentIdentity
	}
	if identity == "" {
		identity = request.IdempotencyKey
	}
	if identity == "" && len(request.RequestHash) > 0 {
		identity = string(request.RequestHash)
	}
	if identity == "" {
		identity = strings.Join([]string{request.TenantID, request.APIKeyID, request.Model, request.ConversationID, request.ExperimentIdentity}, "\x1f")
	}
	digest := sha256.Sum256([]byte(identity + "\x1f" + route.ID))
	value := binary.BigEndian.Uint64(digest[:8])
	unit := (float64(value) + 1) / (float64(math.MaxUint64) + 1)
	return -math.Log(unit) / float64(weight)
}

func routeAcceptsNewAssignments(route Route) bool {
	return route.AdministrativeStatus == "" || route.AdministrativeStatus == RouteActive
}

func routeAcceptsRequest(route Route, preferredRouteID string) bool {
	if route.AdministrativeStatus == RouteDisabled {
		return false
	}
	if route.AdministrativeStatus == RouteDraining {
		return preferredRouteID != "" && preferredRouteID == route.ID
	}
	return true
}

func routeVisibleToTenant(route Route, tenantID string) bool {
	if len(route.TenantIDs) == 0 {
		return true
	}
	for _, allowed := range route.TenantIDs {
		if allowed == tenantID {
			return true
		}
	}
	return false
}

type EchoExecutor struct{}

func NewEchoExecutor() EchoExecutor { return EchoExecutor{} }

func (EchoExecutor) Execute(_ context.Context, request core.Request) (EventStream, error) {
	var input strings.Builder
	for _, item := range request.Input {
		for _, content := range item.Content {
			if content.Type == "input_text" || content.Type == "text" {
				input.WriteString(content.Text)
			}
		}
	}
	text := input.String()
	usage := core.Usage{InputTokens: int64(len(strings.Fields(text))), OutputTokens: int64(len(strings.Fields(text)))}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	return &sliceStream{events: []core.Event{
		{Type: "response.output_text.delta", Delta: text},
		{Type: "response.completed", Usage: &usage, ProviderUsage: json.RawMessage(`{"synthetic":true}`)},
	}}, nil
}

type sliceStream struct {
	events []core.Event
	index  int
}

func (s *sliceStream) Recv() (core.Event, error) {
	if s.index >= len(s.events) {
		return core.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *sliceStream) Close() error { return nil }

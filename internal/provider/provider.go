package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

type Route struct {
	ID                 string
	Provider           string
	Model              string
	Region             string
	CredentialScope    string
	HomeRegion         string
	Healthy            bool
	InputCost          float64
	OutputCost         float64
	Profile            CapabilityProfile
	Executor           ResponseExecutor
	CacheProtector     CacheProtector
	CacheAnchorBuilder CacheAnchorBuilder
	PriceSnapshot      core.PriceSnapshot
	CacheUsageReliable bool
}

type Router interface {
	Candidates(context.Context, core.Request) ([]Route, error)
}

type StaticRouter struct {
	snapshot atomic.Pointer[routeSnapshot]
}

type routeSnapshot struct {
	revision int64
	routes   []Route
}

func NewStaticRouter(executor ResponseExecutor) *StaticRouter {
	return NewVersionedRouter(1, []Route{{
		ID: "echo-default", Provider: "echo", Model: "echo-v1", Region: "local",
		HomeRegion: "local", Healthy: true, Executor: executor,
		Profile: CapabilityProfile{Revision: 1, Features: map[string]CapabilitySupport{
			"text": CapabilityNative, "streaming": CapabilityNative,
		}},
		PriceSnapshot: core.PriceSnapshot{
			ID: "price_echo_v1", Provider: "echo", Model: "echo-v1", Region: "local", Currency: "USD",
			EffectiveAt: 0, Source: "builtin-zero-cost",
		},
		CacheUsageReliable: true,
	}})
}

func NewRouter(routes ...Route) *StaticRouter {
	return NewVersionedRouter(1, routes)
}

func NewVersionedRouter(revision int64, routes []Route) *StaticRouter {
	if revision <= 0 {
		panic("route snapshot revision must be positive")
	}
	router := &StaticRouter{}
	router.snapshot.Store(&routeSnapshot{revision: revision, routes: append([]Route(nil), routes...)})
	return router
}

func (r *StaticRouter) Update(revision int64, routes []Route) error {
	for {
		current := r.snapshot.Load()
		if current != nil && revision <= current.revision {
			return errors.New("route snapshot revision must increase")
		}
		next := &routeSnapshot{revision: revision, routes: append([]Route(nil), routes...)}
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
		if !route.Healthy || (request.Model != route.Model && request.Model != route.ID) {
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
	sort.SliceStable(candidates, func(i, j int) bool {
		if request.PreferredRouteID != "" {
			iPreferred := candidates[i].ID == request.PreferredRouteID
			jPreferred := candidates[j].ID == request.PreferredRouteID
			if iPreferred != jPreferred {
				return iPreferred
			}
		}
		iCost := candidates[i].InputCost + candidates[i].OutputCost
		jCost := candidates[j].InputCost + candidates[j].OutputCost
		return iCost < jCost
	})
	return candidates, nil
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

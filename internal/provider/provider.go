package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
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
	ID              string
	Provider        string
	Model           string
	Region          string
	CredentialScope string
	HomeRegion      string
	Healthy         bool
	InputCost       float64
	OutputCost      float64
	Profile         CapabilityProfile
	Executor        ResponseExecutor
	CacheProtector  CacheProtector
}

type Router interface {
	Candidates(context.Context, core.Request) ([]Route, error)
}

type StaticRouter struct {
	routes []Route
}

func NewStaticRouter(executor ResponseExecutor) *StaticRouter {
	return &StaticRouter{routes: []Route{{
		ID: "echo-default", Provider: "echo", Model: "echo-v1", Region: "local",
		HomeRegion: "local", Healthy: true, Executor: executor,
		Profile: CapabilityProfile{Revision: 1, Features: map[string]CapabilitySupport{
			"text": CapabilityNative, "streaming": CapabilityNative,
		}},
	}}}
}

func NewRouter(routes ...Route) *StaticRouter {
	return &StaticRouter{routes: append([]Route(nil), routes...)}
}

func (r *StaticRouter) Candidates(_ context.Context, request core.Request) ([]Route, error) {
	var candidates []Route
	for _, route := range r.routes {
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
		{Type: "response.completed", Usage: &usage},
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

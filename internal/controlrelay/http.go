package controlrelay

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/operations"
)

const EventPath = "/internal/v1/control-events"

type Handler struct {
	publisher controlevent.Publisher
	verifier  operations.GatewayVerifier
}

func NewHandler(publisher controlevent.Publisher, verifier operations.GatewayVerifier) (*Handler, error) {
	if publisher == nil || verifier == nil {
		return nil, errors.New("Control Event relay requires publisher and Gateway verifier")
	}
	return &Handler{publisher: publisher, verifier: verifier}, nil
}

func (handler *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || request.URL.Path != EventPath {
		http.NotFound(response, request)
		return
	}
	after, err := parseBoundedInt(request.URL.Query().Get("after"), 0, 0, 1<<62)
	if err != nil {
		http.Error(response, "invalid cursor", http.StatusBadRequest)
		return
	}
	limit, err := parseBoundedInt(request.URL.Query().Get("limit"), 256, 1, 256)
	if err != nil {
		http.Error(response, "invalid limit", http.StatusBadRequest)
		return
	}
	identity, err := handler.verifier.Verify(request.Context(), request.Header.Get("Authorization"), request.Method, request.URL.RequestURI(), nil)
	if err != nil {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	batch, err := handler.publisher.Publish(request.Context(), controlevent.Audience{
		GatewayID: identity.GatewayID, Region: identity.Region,
	}, after, int(limit))
	if err != nil {
		slog.Warn("Control Event relay publication failed", "error_code", "control_event_publication_failed", "error", err)
		http.Error(response, "control event relay unavailable", http.StatusServiceUnavailable)
		return
	}
	if batch.Events == nil {
		batch.Events = []controlevent.Event{}
	}
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(batch)
}

func parseBoundedInt(value string, fallback, minimum, maximum int64) (int64, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, errors.New("bounded integer required")
	}
	return parsed, nil
}

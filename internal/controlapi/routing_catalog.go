package controlapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/toddzheng/llm-gateway/internal/providerconnection"
	"github.com/toddzheng/llm-gateway/internal/routingcatalog"
	"github.com/toddzheng/llm-gateway/internal/tenantadmin"
)

func (server *Server) routeRoutingCatalog(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope) bool {
	const catalog = "/control/v1/routing-catalog"
	const publications = "/control/v1/routing-publications"
	if request.URL.Path == publications || request.URL.Path == publications+"/" {
		return false
	}
	if strings.HasPrefix(request.URL.Path, publications+"/") {
		if server.routingCatalog == nil {
			writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "Routing Catalog Administration is not configured")
			return true
		}
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return true
		}
		publicationID, err := onePathID(strings.TrimPrefix(request.URL.Path, publications+"/"))
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", "invalid routing publication ID")
			return true
		}
		publication, err := server.routingCatalog.GetPublication(request.Context(), actor, publicationID)
		if err != nil {
			writeRoutingCatalogError(writer, err)
			return true
		}
		writeJSON(writer, http.StatusOK, publication)
		return true
	}
	if request.URL.Path != catalog && request.URL.Path != catalog+"/" && !strings.HasPrefix(request.URL.Path, catalog+"/") {
		return false
	}
	if server.routingCatalog == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "Routing Catalog Administration is not configured")
		return true
	}
	if request.URL.Path == catalog || request.URL.Path == catalog+"/" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return true
		}
		revision, err := server.routingCatalog.Current(request.Context(), actor)
		if err != nil {
			writeRoutingCatalogError(writer, err)
			return true
		}
		writer.Header().Set("ETag", etag(revision.Revision))
		writeJSON(writer, http.StatusOK, revision)
		return true
	}
	tail := strings.Split(strings.TrimPrefix(request.URL.Path, catalog+"/"), "/")
	switch tail[0] {
	case "drafts":
		server.routeRoutingDrafts(writer, request, actor, tail[1:])
	case "revisions":
		server.routeRoutingRevisions(writer, request, actor, tail[1:])
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
	}
	return true
}

func (server *Server) routeRoutingDrafts(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tail []string) {
	if len(tail) == 0 {
		if request.Method == http.MethodGet {
			server.listRoutingDrafts(writer, request, actor)
			return
		}
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodGet, http.MethodPost)
			return
		}
		var input struct {
			ID           string                  `json:"id"`
			BaseRevision int64                   `json:"base_revision"`
			Document     routingcatalog.Document `json:"document"`
			Reason       string                  `json:"reason"`
		}
		if !decodeBody(writer, request, &input) {
			return
		}
		actor.Reason = input.Reason
		result, err := server.routingCatalog.CreateDraft(request.Context(), actor, request.Header.Get("Idempotency-Key"), routingcatalog.CreateDraftCommand{
			ID: input.ID, BaseRevision: input.BaseRevision, Document: input.Document,
		})
		if err != nil {
			writeRoutingCatalogError(writer, err)
			return
		}
		writeDraftHeaders(writer, result)
		writer.Header().Set("Location", "/control/v1/routing-catalog/drafts/"+url.PathEscape(result.Draft.ID))
		writeJSON(writer, http.StatusCreated, result.Draft)
		return
	}
	draftID, err := onePathID(tail[0])
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "invalid Routing Catalog draft ID")
		return
	}
	if len(tail) == 1 {
		switch request.Method {
		case http.MethodGet:
			draft, err := server.routingCatalog.GetDraft(request.Context(), actor, draftID)
			if err != nil {
				writeRoutingCatalogError(writer, err)
				return
			}
			writer.Header().Set("ETag", etag(draft.Revision))
			writeJSON(writer, http.StatusOK, draft)
		case http.MethodPut:
			var input struct {
				Document         routingcatalog.Document `json:"document"`
				ExpectedRevision int64                   `json:"expected_revision"`
				Reason           string                  `json:"reason"`
			}
			if !decodeBody(writer, request, &input) {
				return
			}
			revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
			if !ok {
				return
			}
			actor.Reason = input.Reason
			result, err := server.routingCatalog.UpdateDraft(request.Context(), actor, request.Header.Get("Idempotency-Key"), routingcatalog.UpdateDraftCommand{
				DraftID: draftID, ExpectedRevision: revision, Document: input.Document,
			})
			if err != nil {
				writeRoutingCatalogError(writer, err)
				return
			}
			writeDraftHeaders(writer, result)
			writeJSON(writer, http.StatusOK, result.Draft)
		default:
			methodNotAllowed(writer, http.MethodGet, http.MethodPut)
		}
		return
	}
	if len(tail) != 2 || request.Method != http.MethodPost {
		if len(tail) == 2 {
			methodNotAllowed(writer, http.MethodPost)
		} else {
			writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
		}
		return
	}
	var input struct {
		ExpectedRevision int64    `json:"expected_revision"`
		RequiredRegions  []string `json:"required_regions"`
		Reason           string   `json:"reason"`
	}
	if !decodeBody(writer, request, &input) {
		return
	}
	revision, ok := expectedRevision(writer, request, input.ExpectedRevision)
	if !ok {
		return
	}
	actor.Reason = input.Reason
	switch tail[1] {
	case "validate":
		result, err := server.routingCatalog.ValidateDraft(request.Context(), actor, routingcatalog.ValidateDraftCommand{DraftID: draftID, ExpectedRevision: revision})
		if err != nil {
			writeRoutingCatalogError(writer, err)
			return
		}
		writeDraftHeaders(writer, result)
		writeJSON(writer, http.StatusOK, result.Draft)
	case "probe":
		server.probeRoutingDraft(writer, request, actor, draftID, revision)
	case "publish":
		result, err := server.routingCatalog.PublishDraft(request.Context(), actor, request.Header.Get("Idempotency-Key"), routingcatalog.PublishDraftCommand{
			DraftID: draftID, ExpectedRevision: revision, RequiredRegions: input.RequiredRegions,
		})
		if err != nil {
			writeRoutingCatalogError(writer, err)
			return
		}
		writePublication(writer, result)
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
	}
}

func (server *Server) listRoutingDrafts(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope) {
	limit, err := optionalInt(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "limit must be an integer")
		return
	}
	page, err := server.routingCatalog.ListDrafts(request.Context(), actor, routingcatalog.DraftFilter{
		Status: routingcatalog.DraftStatus(request.URL.Query().Get("status")), Cursor: request.URL.Query().Get("cursor"), Limit: limit,
	})
	if err != nil {
		writeRoutingCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, page)
}

func (server *Server) probeRoutingDraft(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, draftID string, expectedRevision int64) {
	if server.providerConnections == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "Provider Connection operations are not configured")
		return
	}
	idempotencyKey := request.Header.Get("Idempotency-Key")
	if len(idempotencyKey) == 0 || len(idempotencyKey) > 255 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "Idempotency-Key must contain 1 to 255 characters")
		return
	}
	draft, err := server.routingCatalog.GetDraft(request.Context(), actor, draftID)
	if err != nil {
		writeRoutingCatalogError(writer, err)
		return
	}
	if draft.Revision != expectedRevision {
		writeRoutingCatalogError(writer, routingcatalog.ErrRevisionConflict)
		return
	}
	connectionIDs := make(map[string]struct{}, len(draft.Document.Routes))
	for _, route := range draft.Document.Routes {
		connectionIDs[route.ProviderConnectionID] = struct{}{}
	}
	ordered := make([]string, 0, len(connectionIDs))
	for connectionID := range connectionIDs {
		ordered = append(ordered, connectionID)
	}
	sort.Strings(ordered)
	operations := make([]providerconnection.Operation, 0, len(ordered))
	for _, connectionID := range ordered {
		connection, err := server.providerConnections.Get(request.Context(), actor, connectionID)
		if err != nil {
			writeProviderConnectionError(writer, err)
			return
		}
		result, err := server.providerConnections.RequestProbe(request.Context(), actor,
			routingProbeIdempotencyKey(idempotencyKey, draftID, connectionID),
			providerconnection.OperationCommand{ConnectionID: connectionID, ExpectedRevision: connection.Revision})
		if err != nil {
			writeProviderConnectionError(writer, err)
			return
		}
		operations = append(operations, result.Operation)
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"data": operations})
}

func routingProbeIdempotencyKey(key, draftID, connectionID string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{key, draftID, connectionID}, "\x1f")))
	return "routing-probe-" + hex.EncodeToString(digest[:])
}

func (server *Server) routeRoutingRevisions(writer http.ResponseWriter, request *http.Request, actor tenantadmin.ActorEnvelope, tail []string) {
	if len(tail) == 0 {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		cursor, err := parseInt64(request.URL.Query().Get("cursor"))
		limit, limitErr := optionalInt(request.URL.Query().Get("limit"))
		if err != nil || limitErr != nil {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", "cursor and limit must be integers")
			return
		}
		page, err := server.routingCatalog.ListRevisions(request.Context(), actor, cursor, limit)
		if err != nil {
			writeRoutingCatalogError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, page)
		return
	}
	revision, err := strconv.ParseInt(tail[0], 10, 64)
	if err != nil || revision <= 0 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "invalid Routing Catalog revision")
		return
	}
	if len(tail) == 1 {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, http.MethodGet)
			return
		}
		result, err := server.routingCatalog.GetRevision(request.Context(), actor, revision)
		if err != nil {
			writeRoutingCatalogError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
		return
	}
	if len(tail) != 2 || tail[1] != "restore" || request.Method != http.MethodPost {
		writeAPIError(writer, http.StatusNotFound, "not_found", "route not found")
		return
	}
	var input struct {
		ExpectedHead    int64    `json:"expected_head"`
		RequiredRegions []string `json:"required_regions"`
		Reason          string   `json:"reason"`
	}
	if !decodeBody(writer, request, &input) {
		return
	}
	actor.Reason = input.Reason
	result, err := server.routingCatalog.Restore(request.Context(), actor, request.Header.Get("Idempotency-Key"), routingcatalog.RestoreCommand{
		SourceRevision: revision, ExpectedHead: input.ExpectedHead, RequiredRegions: input.RequiredRegions,
	})
	if err != nil {
		writeRoutingCatalogError(writer, err)
		return
	}
	writePublication(writer, result)
}

func writeDraftHeaders(writer http.ResponseWriter, result routingcatalog.DraftResult) {
	writer.Header().Set("ETag", etag(result.Draft.Revision))
	if result.Replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
}

func writePublication(writer http.ResponseWriter, result routingcatalog.PublicationResult) {
	if result.Replay {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	writer.Header().Set("Location", "/control/v1/routing-publications/"+url.PathEscape(result.Publication.ID))
	writeJSON(writer, http.StatusAccepted, result)
}

func writeRoutingCatalogError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, routingcatalog.ErrNotFound):
		writeAPIError(writer, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, routingcatalog.ErrAlreadyExists):
		writeAPIError(writer, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, routingcatalog.ErrRevisionConflict):
		writeAPIError(writer, http.StatusConflict, "revision_conflict", err.Error())
	case errors.Is(err, routingcatalog.ErrIdempotencyConflict):
		writeAPIError(writer, http.StatusConflict, "idempotency_conflict", err.Error())
	case errors.Is(err, routingcatalog.ErrPolicyDenied):
		writeAPIError(writer, http.StatusForbidden, "policy_denied", err.Error())
	case errors.Is(err, routingcatalog.ErrInvalidArgument), errors.Is(err, routingcatalog.ErrValidationFailed):
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		writeAPIError(writer, http.StatusInternalServerError, "internal_error", "control-plane operation failed")
	}
}

func parseInt64(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	return strconv.ParseInt(value, 10, 64)
}

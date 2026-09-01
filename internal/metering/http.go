package metering

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Identity struct {
	ActorID  string
	TenantID string
	Scopes   []string
}

type IdentityVerifier interface {
	Verify(context.Context, string) (Identity, error)
}
type IdentityVerifierFunc func(context.Context, string) (Identity, error)

func (function IdentityVerifierFunc) Verify(ctx context.Context, value string) (Identity, error) {
	return function(ctx, value)
}

type Handler struct {
	service    *Service
	verifier   IdentityVerifier
	exports    ExportStore
	signingKey []byte
	now        func() time.Time
}

func NewHandler(service *Service, verifier IdentityVerifier, exports ExportStore, signingKey []byte, now func() time.Time) (http.Handler, error) {
	if service == nil || verifier == nil {
		return nil, errors.New("Metering HTTP requires service and identity verifier")
	}
	if now == nil {
		now = time.Now
	}
	return &Handler{service: service, verifier: verifier, exports: exports, signingKey: append([]byte(nil), signingKey...), now: now}, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/readyz" {
		_, err := handler.service.Status(request.Context())
		if err != nil {
			writeError(writer, http.StatusServiceUnavailable, "not_ready")
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"ready": true})
		return
	}
	if strings.HasPrefix(request.URL.Path, "/metering/v1/usage/exports/") && strings.HasSuffix(request.URL.Path, "/content") {
		handler.download(writer, request)
		return
	}
	identity, err := handler.verifier.Verify(request.Context(), request.Header.Get("Authorization"))
	if err != nil || identity.ActorID == "" {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeError(writer, http.StatusUnauthorized, "invalid_identity_assertion")
		return
	}
	path := request.URL.Path
	if request.Method == http.MethodGet && strings.HasPrefix(path, "/metering/v1/tenants/") {
		handler.tenantQuery(writer, request, identity)
		return
	}
	if request.Method == http.MethodPost && strings.HasPrefix(path, "/metering/v1/usage/events/") && strings.HasSuffix(path, "/corrections") {
		if !hasScope(identity, ScopePlatformWrite) {
			writeError(writer, http.StatusForbidden, "policy_denied")
			return
		}
		handler.correct(writer, request, identity)
		return
	}
	if request.Method == http.MethodPost && path == "/metering/v1/admin/rebuilds" {
		if !hasScope(identity, ScopePlatformWrite) {
			writeError(writer, http.StatusForbidden, "policy_denied")
			return
		}
		generation, err := handler.service.Rebuild(request.Context())
		handler.resultStatus(writer, map[string]any{"generation": generation}, err, http.StatusAccepted)
		return
	}
	if request.Method == http.MethodGet && path == "/metering/v1/operations/status" {
		if !hasScope(identity, ScopePlatformRead) && !hasScope(identity, ScopePlatformWrite) {
			writeError(writer, http.StatusForbidden, "policy_denied")
			return
		}
		result, err := handler.service.Status(request.Context())
		handler.result(writer, result, err)
		return
	}
	filter, err := filterFromRequest(request, identity)
	if err != nil {
		if errors.Is(err, ErrInvalidArgument) {
			writeError(writer, http.StatusBadRequest, "invalid_request")
		} else {
			writeError(writer, http.StatusForbidden, "policy_denied")
		}
		return
	}
	switch {
	case request.Method == http.MethodGet && path == "/metering/v1/quota-denials":
		limit, err := optionalQueryInt(request.URL.Query().Get("limit"))
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid_request")
			return
		}
		result, err := handler.service.QuotaDenials(request.Context(), QuotaDenialFilter{
			Filter: filter, Scope: request.URL.Query().Get("scope"), Dimension: request.URL.Query().Get("dimension"),
			Cursor: request.URL.Query().Get("cursor"), Limit: limit,
		})
		handler.result(writer, result, err)
	case request.Method == http.MethodGet && path == "/metering/v1/usage/summary":
		result, err := handler.service.Summary(request.Context(), filter)
		handler.result(writer, result, err)
	case request.Method == http.MethodGet && path == "/metering/v1/usage/timeseries":
		result, err := handler.service.TimeSeries(request.Context(), filter, request.URL.Query().Get("granularity"))
		handler.result(writer, map[string]any{"data": result}, err)
	case request.Method == http.MethodGet && path == "/metering/v1/usage/events":
		limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
		result, err := handler.service.Events(request.Context(), filter, request.URL.Query().Get("cursor"), limit)
		handler.result(writer, result, err)
	case path == "/metering/v1/usage/exports" && request.Method == http.MethodPost:
		result, err := handler.service.RequestExport(request.Context(), filter)
		handler.resultStatus(writer, result, err, http.StatusAccepted)
	case path == "/metering/v1/usage/exports" && request.Method == http.MethodGet:
		limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
		result, err := handler.service.ListExports(request.Context(), filter.TenantID, limit)
		handler.result(writer, map[string]any{"data": result}, err)
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/metering/v1/usage/exports/"):
		id := strings.TrimPrefix(path, "/metering/v1/usage/exports/")
		result, err := handler.service.GetExport(request.Context(), filter.TenantID, id)
		if err == nil && result.Status == "succeeded" && handler.exports != nil && len(handler.signingKey) >= 32 {
			writer.Header().Set("Link", `<`+handler.signedDownload(result)+`>; rel="content"`)
		}
		handler.result(writer, result, err)
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/metering/v1/responses/") && strings.HasSuffix(path, "/usage"):
		filter.ResponseID = strings.TrimSuffix(strings.TrimPrefix(path, "/metering/v1/responses/"), "/usage")
		if filter.ResponseID == "" || strings.Contains(filter.ResponseID, "/") {
			writeError(writer, http.StatusNotFound, "not_found")
			return
		}
		result, err := handler.service.Events(request.Context(), filter, "", 200)
		handler.result(writer, result, err)
	default:
		writeError(writer, http.StatusNotFound, "not_found")
	}
}

func optionalQueryInt(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	return strconv.Atoi(value)
}

func filterFromRequest(request *http.Request, identity Identity) (Filter, error) {
	query := request.URL.Query()
	filter := Filter{TenantID: query.Get("tenant_id"), APIKeyID: query.Get("api_key_id"), ResponseID: query.Get("response_id"), Provider: query.Get("provider"), PublicModel: query.Get("public_model"), ProviderModel: query.Get("provider_model"), RouteID: query.Get("route_id"), Outcome: query.Get("outcome"), Currency: query.Get("currency")}
	if value := query.Get("from"); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return Filter{}, fmt.Errorf("%w: invalid from timestamp", ErrInvalidArgument)
		}
		filter.From = parsed
	}
	if value := query.Get("through"); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return Filter{}, fmt.Errorf("%w: invalid through timestamp", ErrInvalidArgument)
		}
		filter.Through = parsed
	}
	if hasScope(identity, ScopePlatformRead) || hasScope(identity, ScopePlatformWrite) {
		filter.AllTenants = filter.TenantID == ""
		return filter, nil
	}
	if !hasScope(identity, ScopeTenantRead) || identity.TenantID == "" {
		return Filter{}, ErrPolicyDenied
	}
	if filter.TenantID != "" && filter.TenantID != identity.TenantID {
		return Filter{}, ErrPolicyDenied
	}
	filter.TenantID = identity.TenantID
	return filter, nil
}

func (handler *Handler) tenantQuery(writer http.ResponseWriter, request *http.Request, identity Identity) {
	remainder := strings.TrimPrefix(request.URL.Path, "/metering/v1/tenants/")
	parts := strings.Split(remainder, "/")
	if len(parts) < 2 {
		writeError(writer, http.StatusNotFound, "not_found")
		return
	}
	tenantID, _ := url.PathUnescape(parts[0])
	copy := request.Clone(request.Context())
	query := copy.URL.Query()
	query.Set("tenant_id", tenantID)
	copy.URL.RawQuery = query.Encode()
	filter, err := filterFromRequest(copy, identity)
	if err != nil {
		writeError(writer, http.StatusForbidden, "policy_denied")
		return
	}
	if len(parts) == 2 && parts[1] == "usage" {
		result, err := handler.service.Summary(request.Context(), filter)
		handler.result(writer, result, err)
		return
	}
	if len(parts) == 4 && parts[1] == "gateway-api-keys" && parts[3] == "usage" {
		filter.APIKeyID = parts[2]
		result, err := handler.service.Summary(request.Context(), filter)
		handler.result(writer, result, err)
		return
	}
	writeError(writer, http.StatusNotFound, "not_found")
}

func (handler *Handler) correct(writer http.ResponseWriter, request *http.Request, identity Identity) {
	id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/metering/v1/usage/events/"), "/corrections")
	var body struct {
		Reason string     `json:"reason"`
		Delta  UsageEvent `json:"delta"`
	}
	if err := decodeJSON(request.Body, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := handler.service.Correct(request.Context(), identity.ActorID, body.Reason, request.Header.Get("Idempotency-Key"), id, body.Delta)
	handler.resultStatus(writer, result, err, http.StatusCreated)
}

func (handler *Handler) signedDownload(export Export) string {
	expires := handler.now().Add(5 * time.Minute).Unix()
	message := export.TenantID + "\n" + export.ID + "\n" + strconv.FormatInt(expires, 10)
	mac := hmac.New(sha256.New, handler.signingKey)
	_, _ = mac.Write([]byte(message))
	return "/metering/v1/usage/exports/" + url.PathEscape(export.ID) + "/content?tenant_id=" + url.QueryEscape(export.TenantID) + "&expires=" + strconv.FormatInt(expires, 10) + "&signature=" + hex.EncodeToString(mac.Sum(nil))
}

func (handler *Handler) download(writer http.ResponseWriter, request *http.Request) {
	if handler.exports == nil || len(handler.signingKey) < 32 {
		writeError(writer, http.StatusNotFound, "not_found")
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/metering/v1/usage/exports/"), "/content")
	tenant := request.URL.Query().Get("tenant_id")
	expires, _ := strconv.ParseInt(request.URL.Query().Get("expires"), 10, 64)
	signature, err := hex.DecodeString(request.URL.Query().Get("signature"))
	message := tenant + "\n" + id + "\n" + strconv.FormatInt(expires, 10)
	mac := hmac.New(sha256.New, handler.signingKey)
	_, _ = mac.Write([]byte(message))
	if err != nil || expires < handler.now().Unix() || !hmac.Equal(signature, mac.Sum(nil)) {
		writeError(writer, http.StatusForbidden, "invalid_download_signature")
		return
	}
	job, err := handler.service.GetExport(request.Context(), tenant, id)
	if err != nil || job.Status != "succeeded" {
		writeError(writer, http.StatusNotFound, "not_found")
		return
	}
	payload, err := handler.exports.Get(request.Context(), job.ObjectKey)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "export_unavailable")
		return
	}
	digest := sha256.Sum256(payload)
	if !hmac.Equal([]byte(hex.EncodeToString(digest[:])), []byte(job.SHA256)) {
		writeError(writer, http.StatusServiceUnavailable, "export_integrity_failed")
		return
	}
	writer.Header().Set("Content-Type", "text/csv")
	writer.Header().Set("Cache-Control", "private, no-store")
	writer.Header().Set("Content-Disposition", `attachment; filename="`+id+`.csv"`)
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(payload)
}

func (handler *Handler) result(writer http.ResponseWriter, value any, err error) {
	handler.resultStatus(writer, value, err, http.StatusOK)
}
func (handler *Handler) resultStatus(writer http.ResponseWriter, value any, err error, status int) {
	if err == nil {
		writeJSON(writer, status, value)
		return
	}
	switch {
	case errors.Is(err, ErrInvalidArgument):
		writeError(writer, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, ErrNotFound):
		writeError(writer, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrPolicyDenied):
		writeError(writer, http.StatusForbidden, "policy_denied")
	default:
		writeError(writer, http.StatusInternalServerError, "internal_error")
	}
}
func hasScope(identity Identity, scope string) bool {
	for _, candidate := range identity.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}
func decodeJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
func writeError(writer http.ResponseWriter, status int, code string) {
	message := map[string]string{
		"invalid_identity_assertion": "identity assertion is missing or invalid",
		"policy_denied":              "Metering access is denied",
		"invalid_request":            "Metering request is invalid",
		"not_found":                  "Metering resource was not found",
		"conflict":                   "Metering request conflicts with existing state",
		"not_ready":                  "Metering is not ready",
		"internal_error":             "Metering operation failed",
	}[code]
	if message == "" {
		message = "Metering request failed"
	}
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

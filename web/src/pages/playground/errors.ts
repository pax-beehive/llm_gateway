import { ApiError } from "../../api/client";

/**
 * Map gateway/BFF error envelopes to operator-friendly copy. Keyed on the
 * envelope code first, with the HTTP status as a fallback so unlisted codes
 * still land on a sensible bucket.
 */
export function friendlyFor(status: number, code: string, fallback: string): string {
  if (status === 503 && code === "upstream_not_configured") {
    return "gateway upstream not configured on the BFF";
  }
  switch (code) {
    case "invalid_request_error":
    case "route_not_found":
      return "request or model unavailable";
    case "authentication_error":
      return "authentication failed — check BFF/gateway key config";
    case "policy_denied":
      return "denied by tenant policy";
    case "rate_limit_exceeded":
      return "rate limit — retry manually later";
    case "provider_unavailable":
    case "gateway_error":
      return "provider unavailable";
    case "policy_coordination_unavailable":
      return "service temporarily unavailable";
    default:
      break;
  }
  if (status === 400) return "request or model unavailable";
  if (status === 401) return "authentication failed — check BFF/gateway key config";
  if (status === 403) return "denied by tenant policy";
  if (status === 429) return "rate limit — retry manually later";
  if (status === 502) return "provider unavailable";
  if (status === 503) return "service temporarily unavailable";
  return fallback;
}

export function friendlyApiError(err: ApiError): string {
  return friendlyFor(err.status, err.code, err.message);
}

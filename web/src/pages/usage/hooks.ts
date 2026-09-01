import { useCallback, useEffect, useState } from "react";
import { apiGet, ApiError } from "../../api/client";

export interface QueryState<T> {
  data: T | null;
  error: ApiError | null;
  loading: boolean;
  retry: () => void;
}

/**
 * Like useApi, but skips the request while `path` is null (e.g. a required
 * tenant scope has not been entered yet) and keeps previous data on refetch.
 */
export function useQuery<T>(path: string | null, refreshKey = 0): QueryState<T> {
  const [state, setState] = useState<{ data: T | null; error: ApiError | null; loading: boolean }>({
    data: null,
    error: null,
    loading: path !== null,
  });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    if (path === null) {
      setState({ data: null, error: null, loading: false });
      return;
    }
    const controller = new AbortController();
    setState((s) => ({ ...s, loading: s.data === null, error: null }));
    apiGet<T>(path, { signal: controller.signal })
      .then((data) => {
        if (!controller.signal.aborted) setState({ data, error: null, loading: false });
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) return;
        setState({
          data: null,
          error: err instanceof ApiError ? err : new ApiError(0, "network", String(err)),
          loading: false,
        });
      });
    return () => controller.abort();
  }, [path, attempt, refreshKey]);

  const retry = useCallback(() => setAttempt((a) => a + 1), []);
  return { ...state, retry };
}

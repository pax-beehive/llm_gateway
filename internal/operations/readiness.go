package operations

import (
	"context"
	"net/http"
	"sort"
	"time"
)

type Check func(context.Context) error

type Probe struct {
	checks  map[string]Check
	timeout time.Duration
	now     func() time.Time
}

func NewProbe(timeout time.Duration, now func() time.Time, checks map[string]Check) *Probe {
	if timeout <= 0 {
		timeout = 750 * time.Millisecond
	}
	if now == nil {
		now = time.Now
	}
	copyChecks := make(map[string]Check, len(checks))
	for name, check := range checks {
		copyChecks[name] = check
	}
	return &Probe{checks: copyChecks, timeout: timeout, now: now}
}

func (probe *Probe) Check(ctx context.Context) ReadinessResult {
	ctx, cancel := context.WithTimeout(ctx, probe.timeout)
	defer cancel()
	type outcome struct {
		name string
		ok   bool
	}
	results := make(chan outcome, len(probe.checks))
	for name, check := range probe.checks {
		go func(name string, check Check) {
			if check == nil {
				results <- outcome{name: name}
				return
			}
			err := check(ctx)
			results <- outcome{name: name, ok: err == nil}
		}(name, check)
	}
	seen := make(map[string]bool, len(probe.checks))
	for count := 0; count < len(probe.checks); count++ {
		select {
		case result := <-results:
			seen[result.name] = result.ok
		case <-ctx.Done():
			count = len(probe.checks)
		}
	}
	names := make([]string, 0, len(probe.checks))
	for name := range probe.checks {
		names = append(names, name)
	}
	sort.Strings(names)
	readiness := ReadinessResult{Ready: true, Checked: probe.now().UTC(), Checks: make([]CheckResult, 0, len(names))}
	for _, name := range names {
		status := "ready"
		if !seen[name] {
			status = "unavailable"
			readiness.Ready = false
		}
		readiness.Checks = append(readiness.Checks, CheckResult{Name: name, Status: status})
	}
	return readiness
}

func SchemaCompatible(current, minimum, maximum int) bool {
	return current >= minimum && current <= maximum && minimum > 0 && maximum >= minimum
}

func Handler(next http.Handler, readiness ReadinessChecker) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
			writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/readyz" {
			if readiness == nil {
				writeJSON(writer, http.StatusServiceUnavailable, ReadinessResult{Ready: false, Checks: []CheckResult{{Name: "configuration", Status: "unavailable"}}, Checked: time.Now().UTC()})
				return
			}
			result := readiness.Check(request.Context())
			status := http.StatusOK
			if !result.Ready {
				status = http.StatusServiceUnavailable
			}
			writeJSON(writer, status, result)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

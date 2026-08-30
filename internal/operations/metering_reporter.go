package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const meteringObservationPath = "/internal/v1/operations/metering-observations"

type MeteringReporter struct {
	endpoint *url.URL
	id       string
	region   string
	key      []byte
	client   *http.Client
	now      func() time.Time
}

func NewMeteringReporter(endpoint, id, region string, key []byte, client *http.Client, now func() time.Time) (*MeteringReporter, error) {
	parsed, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || !resourceID.MatchString(id) || strings.TrimSpace(region) == "" || len(key) < 32 {
		return nil, errors.New("Metering Operations reporter configuration is invalid")
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	return &MeteringReporter{endpoint: parsed, id: id, region: region, key: append([]byte(nil), key...), client: client, now: now}, nil
}

func (reporter *MeteringReporter) Report(ctx context.Context, observation MeteringObservation) error {
	observation.EventSchemaVersion = CurrentMeteringObservationVersion
	observation.MeteringID = reporter.id
	observation.Region = reporter.region
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = reporter.now().UTC()
	}
	body, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	target := *reporter.endpoint
	target.Path = strings.TrimRight(target.Path, "/") + meteringObservationPath
	target.RawQuery = ""
	authorization, err := MeteringAuthorization(reporter.key, reporter.id, reporter.now().UTC(), http.MethodPost, target.RequestURI(), body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Content-Type", "application/json")
	response, err := reporter.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("Metering Operations observation status %d", response.StatusCode)
	}
	return nil
}

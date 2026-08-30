package controlrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/toddzheng/llm-gateway/internal/controlevent"
	"github.com/toddzheng/llm-gateway/internal/operations"
)

type Client struct {
	endpoint  *url.URL
	gatewayID string
	key       []byte
	client    *http.Client
	now       func() time.Time
}

func NewClient(endpoint, gatewayID string, key []byte, client *http.Client, now func() time.Time) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || gatewayID == "" || len(key) < 32 {
		return nil, errors.New("Control Event relay client configuration is invalid")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else {
		copy := *client
		if copy.Timeout <= 0 || copy.Timeout > 10*time.Second {
			copy.Timeout = 10 * time.Second
		}
		client = &copy
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if now == nil {
		now = time.Now
	}
	return &Client{endpoint: parsed, gatewayID: gatewayID, key: append([]byte(nil), key...), client: client, now: now}, nil
}

func (client *Client) Fetch(ctx context.Context, after int64, limit int) (controlevent.Batch, error) {
	if after < 0 || limit < 1 || limit > 256 {
		return controlevent.Batch{}, errors.New("invalid Control Event fetch request")
	}
	target := *client.endpoint
	target.Path = strings.TrimRight(target.Path, "/") + EventPath
	query := target.Query()
	query.Set("after", strconv.FormatInt(after, 10))
	query.Set("limit", strconv.Itoa(limit))
	target.RawQuery = query.Encode()
	authorization, err := operations.GatewayAuthorization(client.key, client.gatewayID, client.now().UTC(), http.MethodGet, target.RequestURI(), nil)
	if err != nil {
		return controlevent.Batch{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return controlevent.Batch{}, err
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Accept", "application/json")
	response, err := client.client.Do(request)
	if err != nil {
		return controlevent.Batch{}, fmt.Errorf("fetch Control Events: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		var payload struct {
			Error         string `json:"error"`
			MinimumCursor int64  `json:"minimum_cursor"`
		}
		if json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&payload) == nil &&
			payload.Error == "control_event_history_unavailable" && payload.MinimumCursor > after {
			return controlevent.Batch{}, &HistoryUnavailableError{MinimumCursor: payload.MinimumCursor}
		}
		return controlevent.Batch{}, errors.New("invalid Control Event history response")
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return controlevent.Batch{}, fmt.Errorf("Control Event relay status %d", response.StatusCode)
	}
	var batch controlevent.Batch
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err := decoder.Decode(&batch); err != nil {
		return controlevent.Batch{}, errors.New("decode Control Event relay response")
	}
	if batch.NextCursor < after || batch.SourceHead < batch.NextCursor {
		return controlevent.Batch{}, errors.New("Control Event relay cursor regressed")
	}
	previous := after
	for _, event := range batch.Events {
		if event.EventID == "" || event.DeliverySequence <= previous || event.DeliverySequence > batch.NextCursor {
			return controlevent.Batch{}, errors.New("Control Event relay returned an invalid ordered batch")
		}
		previous = event.DeliverySequence
	}
	return batch, nil
}

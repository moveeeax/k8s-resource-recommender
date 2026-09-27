// Package prom is a minimal read-only client for the Prometheus HTTP API.
//
// It deliberately covers only instant queries returning a vector, which is all
// the recommender needs: every usage query is wrapped in an aggregation that
// collapses a range down to one sample per series.
package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sample is one labelled value from an instant-vector result.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Client talks to a Prometheus HTTP API endpoint.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a Client for baseURL with a sane request timeout.
func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: timeout},
	}
}

// apiResponse mirrors the envelope Prometheus wraps every reply in.
type apiResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Query runs expr as an instant query and returns the resulting vector.
func (c *Client) Query(ctx context.Context, expr string) ([]Sample, error) {
	endpoint := c.BaseURL + "/api/v1/query"
	form := url.Values{"query": []string{expr}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query prometheus at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	// Prometheus reports query errors as 400/422 with a JSON body, so decode
	// first and only fall back to the status code when the body is unusable.
	var out apiResponse
	decErr := json.NewDecoder(resp.Body).Decode(&out)
	if decErr != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("prometheus returned HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("decode response: %w", decErr)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus error (%s): %s", out.ErrorType, out.Error)
	}
	if out.Data.ResultType != "vector" {
		return nil, fmt.Errorf("expected a vector result, got %q", out.Data.ResultType)
	}

	samples := make([]Sample, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		// A vector element is [<unix ts>, "<value>"]; only the value matters.
		if len(r.Value) != 2 {
			return nil, fmt.Errorf("malformed sample for %v: expected 2 fields, got %d", r.Metric, len(r.Value))
		}
		var raw string
		if err := json.Unmarshal(r.Value[1], &raw); err != nil {
			return nil, fmt.Errorf("sample value for %v is not a string: %w", r.Metric, err)
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("sample value %q for %v is not a number: %w", raw, r.Metric, err)
		}
		// Prometheus emits "NaN" for series with no data in the window. Those
		// are not an error, there is just nothing to recommend from them.
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		samples = append(samples, Sample{Labels: r.Metric, Value: v})
	}
	return samples, nil
}

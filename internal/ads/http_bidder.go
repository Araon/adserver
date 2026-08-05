package ads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const maxDSPResponseBytes = 64 << 10

// HTTPBidderAdapter sends a DSP request to an HTTP endpoint.
type HTTPBidderAdapter struct {
	bidderID      string
	endpoint      string
	authorization string
	client        *http.Client
}

func NewHTTPBidderAdapter(id, endpoint, authorization string) (*HTTPBidderAdapter, error) {
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid DSP endpoint for %s", id)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.MaxConnsPerHost = 64, 32, 32
	return &HTTPBidderAdapter{bidderID: id, endpoint: endpoint, authorization: authorization, client: &http.Client{Transport: transport}}, nil
}

func (a *HTTPBidderAdapter) ID() string { return a.bidderID }

func (a *HTTPBidderAdapter) Fetch(ctx context.Context, request DSPRequest) ([]Bid, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	if a.authorization != "" {
		httpRequest.Header.Set("Authorization", a.authorization)
	}
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("DSP %s returned HTTP %d", a.bidderID, response.StatusCode)
	}
	payload, err = io.ReadAll(io.LimitReader(response.Body, maxDSPResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read DSP %s response: %w", a.bidderID, err)
	}
	if len(payload) > maxDSPResponseBytes {
		return nil, fmt.Errorf("DSP %s response exceeds %d bytes", a.bidderID, maxDSPResponseBytes)
	}
	var body struct {
		Bids []Bid `json:"bids"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("decode DSP %s response: %w", a.bidderID, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("invalid trailing DSP %s response data", a.bidderID)
	}
	return body.Bids, nil
}

package bili

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"comment-bridge/internal/commentbridge"
)

type riskTransport struct {
	base http.RoundTripper
	stop *commentbridge.BilibiliRiskStop
	mu   sync.Mutex
}

func (t *riskTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Hold through body classification, so queued calls cannot slip past a risk response.
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.stop.BeginRequest(req.Context()); err != nil {
		return nil, err
	}
	resp, requestErr := t.base.RoundTrip(req)
	var status, code int
	var body []byte
	var readErr error
	if resp != nil {
		status = resp.StatusCode
		if !riskHTTPStatus(status) {
			body, readErr = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			var envelope struct {
				Code int `json:"code"`
			}
			if json.Unmarshal(body, &envelope) == nil {
				code = envelope.Code
			}
		}
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}
	stopErr := t.stop.EndRequest(riskHTTPStatus(status) || riskAPICode(code), status, code)
	if stopErr != nil && (riskHTTPStatus(status) || riskAPICode(code)) {
		return nil, stopErr
	}
	if requestErr != nil {
		return nil, requestErr
	}
	if readErr != nil {
		return nil, readErr
	}
	return resp, nil
}

func riskHTTPStatus(code int) bool {
	return code == http.StatusForbidden || code == http.StatusPreconditionFailed || code == http.StatusTooManyRequests
}

func riskAPICode(code int) bool {
	switch code {
	case -352, -403, -412, 412, -509, -102:
		return true
	}
	return false
}

func (c *Client) riskStopped() bool {
	t, ok := c.http.Transport.(*riskTransport)
	return ok && t.stop.Stopped()
}

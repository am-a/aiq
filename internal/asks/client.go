package asks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	modelEndpoint    = "https://api.typesafe.ai/v1/systemone"
	modelName        = "jev-latest"
	modelTimeout     = 15 * time.Second
	maxModelResponse = 1 << 20
	// The published System One schema calls question text instructions.
	instructionsKey = "instructions"
	trueCriterion   = "true"
	falseCriterion  = "false"
)

// Client sends only the supplied text and questions, never session metadata.
// Endpoint and HTTPClient permit a local stub without calling the live service.
type Client struct {
	Endpoint   string
	HTTPClient *http.Client
	key        string
}

func NewClient(key string) *Client {
	return &Client{Endpoint: modelEndpoint, HTTPClient: &http.Client{Timeout: modelTimeout}, key: key}
}

// Ask keeps the deadline alive through reading the response. Errors deliberately
// discard transport, URL, JSON and response details, which can contain input.
func (c *Client) Ask(ctx context.Context, text string, questions map[string]Question) (map[string]float64, error) {
	if strings.TrimSpace(c.key) == "" {
		return nil, &ModelError{Class: ClassAuth}
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.User != nil || u.Host == "" || u.Fragment != "" || !allowedEndpoint(u) {
		return nil, &ModelError{Class: ClassServer}
	}
	if len(questions) == 0 {
		return nil, &ModelError{Class: ClassRejected}
	}
	wire := make(map[string]any, len(questions))
	for name, q := range questions {
		wire[name] = map[string]any{"type": "noul", instructionsKey: cleanLimit(q.Instructions, maxTextBytes),
			"criteria": map[string]string{trueCriterion: cleanLimit(q.True, maxTextBytes), falseCriterion: cleanLimit(q.False, maxTextBytes)}}
	}
	body, err := json.Marshal(map[string]any{"state": cleanLimit(text, maxTextBytes), "model": modelName, "questions": wire})
	if err != nil {
		return nil, &ModelError{Class: ClassRejected}
	}
	ctx, cancel := context.WithTimeout(ctx, modelTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, &ModelError{Class: ClassServer}
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: modelTimeout}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, &ModelError{Class: ClassServer}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		me := &ModelError{Class: ClassServer, Status: resp.StatusCode}
		switch resp.StatusCode {
		case 401, 402, 403, 404:
			me.Class = ClassAuth
		case 429:
			me.Class, me.RetryAfter = ClassRate, retryAfter(resp.Header.Get("Retry-After"), time.Now())
		case 400, 413, 422:
			me.Class = ClassRejected
		}
		return nil, me
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxModelResponse+1))
	if err != nil || ctx.Err() != nil {
		return nil, &ModelError{Class: ClassServer}
	}
	if len(b) > maxModelResponse {
		return nil, &ModelError{Class: ClassMalformed}
	}
	var result struct {
		Answers map[string]struct {
			Noul *float64 `json:"noul"`
			Type string   `json:"type"`
		} `json:"answers"`
	}
	if json.Unmarshal(b, &result) != nil {
		return nil, &ModelError{Class: ClassMalformed}
	}
	out := make(map[string]float64, len(questions))
	for name := range questions {
		a, ok := result.Answers[name]
		if !ok || a.Noul == nil || a.Type != "" && a.Type != "noul" || !probability(*a.Noul) {
			return nil, &ModelError{Class: ClassMalformed}
		}
		out[name] = *a.Noul
	}
	return out, nil
}

func allowedEndpoint(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func probability(p float64) bool { return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= 1 }

func retryAfter(value string, now time.Time) time.Duration {
	const cap = time.Hour
	if seconds, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64); err == nil {
		if seconds > uint64(cap/time.Second) {
			return cap
		}
		return time.Duration(seconds) * time.Second
	} else if n, ok := err.(*strconv.NumError); ok && n.Err == strconv.ErrRange {
		return cap
	}
	if at, err := http.ParseTime(value); err == nil {
		d := at.Sub(now)
		if d > cap {
			return cap
		}
		if d > 0 {
			return d
		}
	}
	return 0
}

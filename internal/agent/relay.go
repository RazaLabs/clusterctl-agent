package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/RazaLabs/kubctl-agent/internal/pairing"
	"github.com/RazaLabs/kubctl-agent/internal/rules"
)

// PushMessage is one notification as the relay takes it. Title and Body
// are what the phone shows; with details off they're generic and the
// problem only exists inside Sealed.
type PushMessage struct {
	ID     string `json:"id"`
	State  string `json:"state"` // firing | resolved
	Title  string `json:"title"`
	Body   string `json:"body"`
	Sealed string `json:"sealed"`
}

func pushFor(p pairing.Pairing, a rules.Alert, resolved, details bool) (PushMessage, error) {
	state := "firing"
	if resolved {
		state = "resolved"
	}
	sealed, err := pairing.Seal(p.SealKey, map[string]any{"cluster": p.ClusterID(), "state": state, "alert": a})
	if err != nil {
		return PushMessage{}, err
	}
	var body string
	switch {
	case details && resolved:
		body = "Resolved: " + a.Title + "\n" + a.Detail
	case details:
		body = a.Title + "\n" + a.Detail
	case resolved:
		body = "Resolved problem. Open kubctl to see it."
	case a.Severity == rules.Critical:
		body = "Critical problem. Open kubctl to see it."
	default:
		body = "Warning problem. Open kubctl to see it."
	}
	return PushMessage{ID: a.ID, State: state, Title: p.Cluster, Body: body, Sealed: sealed}, nil
}

// RelayClient sends notifications to the relay in order, keeping them
// queued (up to a limit) while it can't be reached.
type RelayClient struct {
	pairing pairing.Pairing
	http    *http.Client

	mu            sync.Mutex
	queue         []PushMessage
	kick          chan struct{}
	lastDelivered time.Time
	lastError     string
}

const maxQueue = 500

// NewRelayClient trusts the system's CAs, plus caFile if given (a relay
// with a private CA, or mkcert in development).
func NewRelayClient(p pairing.Pairing, caFile string) (*RelayClient, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("RELAY_CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("RELAY_CA: no certificates in %s", caFile)
		}
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &RelayClient{pairing: p, http: &http.Client{Transport: t, Timeout: 15 * time.Second}, kick: make(chan struct{}, 1)}, nil
}

func (c *RelayClient) Send(m PushMessage) {
	c.mu.Lock()
	c.queue = append(c.queue, m)
	// Drop the oldest if the relay has been away a long time.
	if len(c.queue) > maxQueue {
		c.queue = c.queue[len(c.queue)-maxQueue:]
	}
	c.mu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Status is what the state ConfigMap reports about delivery.
func (c *RelayClient) Status() (lastDelivered time.Time, lastError string, queued int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastDelivered, c.lastError, len(c.queue)
}

// Run delivers until ctx ends, backing off up to 5 minutes on failure.
func (c *RelayClient) Run(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
		}
		for {
			c.mu.Lock()
			batch := append([]PushMessage(nil), c.queue[:min(50, len(c.queue))]...)
			c.mu.Unlock()
			if len(batch) == 0 || c.pairing.RelayURL == "" {
				break
			}
			retry, err := c.post(ctx, batch)
			c.mu.Lock()
			if err != nil && retry {
				c.lastError = err.Error()
				c.mu.Unlock()
				slog.Warn("relay unreachable, retrying", "err", err, "in", backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, 5*time.Minute)
				continue
			}
			// Delivered, or refused for good (4xx other than rate limiting).
			c.queue = c.queue[min(len(batch), len(c.queue)):]
			if err != nil {
				c.lastError = err.Error()
			} else {
				c.lastDelivered, c.lastError = time.Now(), ""
			}
			c.mu.Unlock()
			backoff = 2 * time.Second
		}
	}
}

func (c *RelayClient) post(ctx context.Context, batch []PushMessage) (retry bool, err error) {
	body, _ := json.Marshal(map[string]any{"messages": batch})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.pairing.RelayURL+"/v1/push", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.pairing.RelayKey)
	res, err := c.http.Do(req)
	if err != nil {
		return true, err
	}
	defer res.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	switch {
	case res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests:
		return true, fmt.Errorf("relay answered %d: %s", res.StatusCode, text)
	case res.StatusCode >= 400:
		return false, fmt.Errorf("relay refused the alerts (%d): %s", res.StatusCode, text)
	}
	return false, nil
}

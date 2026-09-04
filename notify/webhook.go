package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// defaultTimeout bounds one delivery attempt. A receiver is somebody else's service, so this is
	// generous enough for a slow one and short enough that a hung endpoint cannot hold the drain
	// worker open past a shutdown.
	defaultTimeout = 10 * time.Second

	// responseBodyLimit bounds how much of a receiver's response is read back. Nothing is done with
	// it beyond putting it in an error message, so this only has to be enough to name the problem.
	responseBodyLimit = 1 << 16

	// signatureHeader and timestampHeader carry the HMAC signature and the instant it covers. The
	// timestamp is signed alongside the body so a captured delivery cannot be replayed indefinitely
	// - a receiver that cares rejects one whose timestamp is too old.
	signatureHeader = "X-Hippocampus-Signature"
	timestampHeader = "X-Hippocampus-Timestamp"

	// kindHeader lets a receiver route without parsing the body. It duplicates Delivery.Kind
	// deliberately; the body remains the authority.
	kindHeader = "X-Hippocampus-Callback-Kind"
)

// Config is everything the webhook sink needs. Only URL is required.
type Config struct {
	// URL is the endpoint each delivery is POSTed to.
	URL string

	// Token, when set, is sent as an "Authorization: Bearer" header.
	Token string

	// SigningSecret, when set, keys the HMAC-SHA256 signature header. It is what lets a receiver
	// verify a delivery actually came from this instance, which a bearer token alone does not - a
	// token proves who is calling, a signature proves the body was not altered in between.
	SigningSecret string

	// Timeout bounds one delivery attempt. Zero selects defaultTimeout.
	Timeout time.Duration

	// TLS carries the optional trust options for an https:// endpoint.
	TLS TLSConfig

	// Transport replaces the HTTP transport outright. It exists for tests - a fake receiver that
	// need not listen on a socket - and takes precedence over the TLS block.
	Transport http.RoundTripper
}

// Webhook is the HTTP callback sink: one POST of JSON per delivery.
//
// It holds no queue and no retry state. Delivery either lands or returns an error, and the drain
// worker decides what happens next - which is what keeps the durability guarantee in one place
// (the persisted queue) rather than split across two.
type Webhook struct {
	url    string
	token  string
	secret []byte
	client *http.Client
}

// NewWebhook builds the HTTP sink.
//
// It validates unusable configuration - a missing or unparseable URL, a malformed TLS block - and
// deliberately never dials. An endpoint that happens to be down at startup must not stop the
// service starting: it is a best-effort secondary consumer, and the queue is what makes its outage
// survivable.
func NewWebhook(cfg Config) (*Webhook, error) {
	log.Trace("func() notify.NewWebhook")

	if strings.TrimSpace(cfg.URL) == "" {
		return nil, fmt.Errorf("callbacks.url is required when callbacks.enabled is set")
	}

	parsed, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing callbacks.url %q: %w", cfg.URL, err)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("callbacks.url %q must be an http:// or https:// URL", cfg.URL)
	}

	if parsed.Host == "" {
		return nil, fmt.Errorf("callbacks.url %q names no host", cfg.URL)
	}

	transport, err := buildTransport(cfg)
	if err != nil {
		return nil, err
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,

		// A callback carries a bearer token and a signature. Following a redirect would forward
		// both to whatever the endpoint named, which is a credential leak the operator never
		// agreed to - so a redirect is returned as the response it is, and reported as a non-2xx.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &Webhook{
		url:    cfg.URL,
		token:  cfg.Token,
		secret: []byte(cfg.SigningSecret),
		client: client,
	}, nil
}

// Enabled reports true: a Webhook only exists when one is configured.
func (w *Webhook) Enabled() bool {
	return w != nil
}

// Endpoint returns the configured URL. Used by the topology view, which redacts any credential in
// it before it reaches a response.
func (w *Webhook) Endpoint() string {
	if w == nil {
		return ""
	}

	return w.url
}

// Deliver POSTs one delivery as JSON, returning nil only on a 2xx.
//
// Every non-2xx is an error, including a 4xx. That is deliberate even though a 4xx will usually
// fail again: the alternative is deciding on the receiver's behalf that its rejection is permanent,
// and a 404 during a deployment is exactly the case where retrying is right. A genuinely permanent
// rejection is bounded by the queue's age and row caps instead, which is one policy rather than two.
func (w *Webhook) Deliver(ctx context.Context, delivery Delivery) error {
	log.Trace("func() notify.Deliver")

	// The marshal guard is defensive and currently unreachable, which is why nothing exercises it:
	// Delivery is a concrete tree of strings, numbers and bools, and json.Marshal sanitises invalid
	// UTF-8 rather than refusing it (TestWebhookSanitisesInvalidUTF8 pins that). It is kept because
	// it would become reachable the day Delivery gains a field that is not one of those things.
	body, err := json.Marshal(delivery)
	if err != nil {
		log.Errorf("failed to encode a callback delivery: %s", err.Error())

		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		log.Errorf("failed to build the callback request: %s", err.Error())

		return err
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(kindHeader, string(delivery.Kind))

	if w.token != "" {
		request.Header.Set("Authorization", "Bearer "+w.token)
	}

	if len(w.secret) > 0 {
		timestamp := strconv.FormatInt(time.Now().UnixNano(), 10)

		request.Header.Set(timestampHeader, timestamp)
		request.Header.Set(signatureHeader, "sha256="+sign(w.secret, timestamp, body))
	}

	response, err := w.client.Do(request)
	if err != nil {
		return fmt.Errorf("delivering a callback: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	// Read the body whatever the status: on a failure it names the problem, and on a success
	// draining it is what lets the connection be reused.
	payload, err := io.ReadAll(io.LimitReader(response.Body, responseBodyLimit))
	if err != nil {
		return fmt.Errorf("reading the callback response: %w", err)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("callback endpoint returned status %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}

	return nil
}

// sign returns the hex HMAC-SHA256 of "<timestamp>.<body>" under the secret. The timestamp is
// inside the signature rather than beside it, so it cannot be changed without invalidating it.
func sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)

	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)

	return hex.EncodeToString(mac.Sum(nil))
}

// Compile-time check that Webhook satisfies Notifier.
var _ Notifier = (*Webhook)(nil)

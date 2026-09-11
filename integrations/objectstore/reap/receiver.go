package reap

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"

	"github.com/fastbean-au/hippocampus/notify"
	"github.com/fastbean-au/hippocampus/observability"
)

const (
	// bodyLimit bounds one delivery. A consolidation chunk of five hundred deletions is one
	// delivery, so this is generous for the largest the service will send while still bounding what
	// an unauthenticated caller can make this process allocate.
	bodyLimit = 8 << 20

	// defaultMaxAge is how old a signed delivery may be before it is refused as a replay. It is
	// only consulted when a signing secret is configured, since without one there is nothing to
	// replay.
	defaultMaxAge = 5 * time.Minute

	signatureHeader = "X-Hippocampus-Signature"
	timestampHeader = "X-Hippocampus-Timestamp"
)

// ReceiverConfig configures the callback endpoint.
type ReceiverConfig struct {
	// Reaper carries out what a delivery instructs. Required.
	Reaper *Reaper

	// Token, when set, is required as "Authorization: Bearer <token>" - matching callbacks.token on
	// the service side.
	Token string

	// Secret, when set, keys the HMAC-SHA256 signature check - matching callbacks.signingSecret on
	// the service side. A token proves who is calling; a signature proves the body was not altered
	// in between, which for an instruction to delete data is the more useful of the two.
	Secret string

	// MaxAge bounds how old a signed delivery may be. Non-positive selects defaultMaxAge.
	MaxAge time.Duration
}

// Receiver is the push path: the HTTP endpoint the service's callback sink posts to.
//
// Its status codes are a contract with the service's drain worker, which treats any non-2xx as
// "retry later" and a 2xx as "delivered, forget it". So a delivery this process could not carry out
// must return 5xx - losing it silently is the one outcome the whole durable-callback mechanism
// exists to prevent - while a delivery this process deliberately does not act on (a kind or a cause
// outside its remit) must return 2xx, or the queue would replay it forever.
type Receiver struct {
	reaper *Reaper
	token  string
	secret []byte
	maxAge time.Duration
}

// NewReceiver builds the callback endpoint.
func NewReceiver(cfg ReceiverConfig) (*Receiver, error) {
	if cfg.Reaper == nil {
		return nil, fmt.Errorf("a reaper is required")
	}

	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = defaultMaxAge
	}

	return &Receiver{
		reaper: cfg.Reaper,
		token:  cfg.Token,
		secret: []byte(cfg.Secret),
		maxAge: maxAge,
	}, nil
}

// ServeHTTP accepts one delivery.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	log.Trace("func() reap.Receiver.ServeHTTP")

	if request.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	body, err := io.ReadAll(io.LimitReader(request.Body, bodyLimit))
	if err != nil {
		r.recordDelivery(request, "", OutcomeRejected)
		http.Error(w, "cannot read the request body", http.StatusBadRequest)

		return
	}

	if err := r.verify(request, body); err != nil {
		r.recordDelivery(request, "", OutcomeUnauthorised)

		log.WithError(err).Warn("refused a callback delivery")
		http.Error(w, "unauthorised", http.StatusUnauthorized)

		return
	}

	var delivery notify.Delivery

	if err := json.Unmarshal(body, &delivery); err != nil {
		r.recordDelivery(request, "", OutcomeRejected)

		log.WithError(err).Warn("could not parse a callback delivery")
		http.Error(w, "cannot parse the delivery", http.StatusBadRequest)

		return
	}

	r.handle(w, request, delivery)
}

// handle acts on a parsed delivery.
func (r *Receiver) handle(w http.ResponseWriter, request *http.Request, delivery notify.Delivery) {
	// A stalled cycle is reported by the party that can end it, which is this one: the store has
	// stopped forgetting because deliveries to this endpoint were not being accepted. It is logged
	// rather than acted on - there is nothing to delete - and at Warn because a store that has
	// stopped forgetting is a store filling up.
	if delivery.Kind == notify.KindSleepCompleted && delivery.Cycle != nil && delivery.Cycle.Stalled {
		log.WithField("reason", delivery.Cycle.StalledReason).
			Warn("the store has stopped forgetting: its callback queue is over its caps and this is the receiver it is waiting for")
	}

	if delivery.Kind != notify.KindMemoryForgotten {
		r.recordDelivery(request, string(delivery.Kind), OutcomeIgnored)

		w.WriteHeader(http.StatusOK)

		return
	}

	if !r.reaper.causes.Acts(delivery.Cause) {
		r.recordDelivery(request, string(delivery.Kind), OutcomeIgnored)

		log.WithField("cause", delivery.Cause).
			Debug("ignored a deletion this agent does not act on")

		w.WriteHeader(http.StatusOK)

		return
	}

	ids := make([]string, 0, len(delivery.Items))
	for _, v := range delivery.Items {
		ids = append(ids, v.Id)
	}

	result, err := r.reaper.Reap(request.Context(), PathCallback, ids)
	if err != nil {
		r.recordDelivery(request, string(delivery.Kind), OutcomeRejected)

		log.WithError(err).WithField("items", len(ids)).
			Error("failed to delete the objects behind a forgotten batch; the service will retry the delivery")

		// A 5xx is the only way to keep the instruction alive: the queue replays what it cannot
		// deliver, and the deletes that did succeed are idempotent on replay.
		http.Error(w, "some objects could not be deleted", http.StatusInternalServerError)

		return
	}

	r.recordDelivery(request, string(delivery.Kind), OutcomeAccepted)

	log.WithFields(log.Fields{
		"items":    len(ids),
		"deleted":  result.Deleted,
		"shadowed": result.Shadowed,
		"skipped":  result.Foreign + result.Unmappable,
		"cause":    delivery.Cause,
	}).
		Debug("handled a forgotten-memory delivery")

	w.WriteHeader(http.StatusOK)
}

// verify checks the bearer token and the signature, in that order. Both are optional; configuring
// neither accepts any caller that can reach the port, which the command warns about.
func (r *Receiver) verify(request *http.Request, body []byte) error {
	if r.token != "" {
		token, found := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
		if !found || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(r.token)) != 1 {
			return fmt.Errorf("the bearer token is missing or wrong")
		}
	}

	if len(r.secret) == 0 {
		return nil
	}

	timestamp := request.Header.Get(timestampHeader)
	if timestamp == "" {
		return fmt.Errorf("the delivery carries no %s", timestampHeader)
	}

	nanos, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("the delivery's %s is not a number", timestampHeader)
	}

	// The window is two-sided: a delivery from the future is as suspect as an old one, and a
	// receiver whose clock has drifted should say so rather than accept anything.
	if age := time.Since(time.Unix(0, nanos)); age > r.maxAge || age < -r.maxAge {
		return fmt.Errorf("the delivery is %s old, over the %s replay window", age.Truncate(time.Second), r.maxAge)
	}

	signature, found := strings.CutPrefix(request.Header.Get(signatureHeader), "sha256=")
	if !found {
		return fmt.Errorf("the delivery carries no %s", signatureHeader)
	}

	expected, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		return fmt.Errorf("the delivery's signature is not hexadecimal")
	}

	if !hmac.Equal(expected, sign(r.secret, timestamp, body)) {
		return fmt.Errorf("the delivery's signature does not match")
	}

	return nil
}

// sign reproduces the sink's HMAC: SHA-256 of "<timestamp>.<body>" under the shared secret. The
// timestamp is inside the signature rather than beside it, so it cannot be changed without
// invalidating it.
func sign(secret []byte, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)

	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)

	return mac.Sum(nil)
}

// recordDelivery counts one delivery. The kind is CLAMPED to the closed set the sink can send,
// because it arrives on the wire from a caller this endpoint may not have authenticated yet - an
// attribute taken straight from a request body is an unbounded metric dimension anybody can grow.
func (r *Receiver) recordDelivery(request *http.Request, kind string, outcome string) {
	tel.deliveries.Add(request.Context(), 1, observability.WithGroup(
		attribute.String(attrKind, knownKind(kind)),
		attribute.String(attrOutcome, outcome),
	))
}

func knownKind(kind string) string {
	switch notify.Kind(kind) {

	case notify.KindMemoryForgotten,
		notify.KindEventForgotten,
		notify.KindSleepCompleted,
		notify.KindMemoriesAtRisk:
		return kind

	}

	return "unknown"
}

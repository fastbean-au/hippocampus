// Package gateway is the request-path chokepoint: an HTTP front door for a bucket that reinforces
// the pointer-memory for every object it serves.
//
// # Why a gateway at all
//
// A tap needs somewhere to attach, and for object storage the natural place is whatever already
// stands between a reader and the bucket. If nothing does, this is the smallest thing that can:
// it answers a read, tells the tap about it, and gets out of the way.
//
// # Redirect, not proxy
//
// The default mode issues a presigned URL and answers 302. No payload byte passes through this
// process, so the gateway is a chokepoint for the DECISION to read without being one for the
// bandwidth - which is what makes it deployable in front of a bucket holding terabytes. Proxy mode
// streams the object instead, for a deployment that cannot expose the store's own hostname to
// readers at all; it forwards Range requests, because answering one with a whole body is a wrong
// answer rather than a degraded one.
//
// # What it is not
//
// It is not an authorisation layer for the bucket. A presigned URL grants whoever holds it a read
// of that object until it expires, so anybody who may call this gateway may read anything in the
// bucket it fronts. That is why an inbound token is required unless anonymous access is asked for
// explicitly, and why the redirect's lifetime is short and configurable.
//
// Reinforcement is best-effort and never fails a read. The gateway's job is serving the object; a
// Hippocampus instance that is down should cost a decay clock its update, not a user their file.
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"

	"github.com/fastbean-au/hippocampus/observability"

	"github.com/fastbean-au/hippocampus/integrations/objectstore/keymap"
	"github.com/fastbean-au/hippocampus/integrations/objectstore/objects"
)

// Mode selects how an object read is answered.
type Mode string

const (
	// ModeRedirect presigns the object and answers 302. The default.
	ModeRedirect Mode = "redirect"

	// ModeProxy streams the object through this process.
	ModeProxy Mode = "proxy"
)

// ValidMode reports whether m is one of the two modes, for a command validating a flag.
func ValidMode(m Mode) bool {
	return m == ModeRedirect || m == ModeProxy
}

// maxRecallKeys bounds one POST /recall body. It is generous for the endpoint's purpose - an
// application reporting the result set it just served - and bounds the memory one request can make
// this process allocate.
const maxRecallKeys = 1000

// recallBodyLimit bounds how much of a /recall body is read, for the same reason.
const recallBodyLimit = 1 << 20

// Toucher is the tap, declared as an interface so the gateway can be tested without one.
type Toucher interface {
	Touch(ctx context.Context, id string) error
}

// Config configures a Gateway.
type Config struct {
	// Store is the bucket being fronted. Required.
	Store objects.Store

	// Tap receives the id of every object read. Required.
	Tap Toucher

	// Mode selects redirect or proxy. Empty selects ModeRedirect.
	Mode Mode

	// URLTTL is how long a presigned URL stays valid. Non-positive selects defaultURLTTL.
	URLTTL time.Duration

	// AuthToken, when set, is required as "Authorization: Bearer <token>" on every request. Empty
	// serves anonymously - which the command refuses to do unless it was asked for explicitly.
	AuthToken string

	// PathPrefix is the path objects are served under. Empty selects defaultPathPrefix.
	PathPrefix string
}

const (
	defaultURLTTL     = 5 * time.Minute
	defaultPathPrefix = "/o/"
)

// Gateway serves objects and taps the reads.
type Gateway struct {
	store      objects.Store
	tap        Toucher
	mode       Mode
	urlTTL     time.Duration
	authToken  string
	pathPrefix string
}

// New validates cfg and builds a Gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("a bucket is required")
	}

	if cfg.Tap == nil {
		return nil, fmt.Errorf("a tap is required")
	}

	mode := cfg.Mode
	if mode == "" {
		mode = ModeRedirect
	}

	if !ValidMode(mode) {
		return nil, fmt.Errorf("invalid mode %q (want %q or %q)", mode, ModeRedirect, ModeProxy)
	}

	prefix := cfg.PathPrefix
	if prefix == "" {
		prefix = defaultPathPrefix
	}

	if !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") {
		return nil, fmt.Errorf("the path prefix %q must begin and end with a slash", prefix)
	}

	ttl := cfg.URLTTL
	if ttl <= 0 {
		ttl = defaultURLTTL
	}

	return &Gateway{
		store:      cfg.Store,
		tap:        cfg.Tap,
		mode:       mode,
		urlTTL:     ttl,
		authToken:  cfg.AuthToken,
		pathPrefix: prefix,
	}, nil
}

// Handler returns the routes: the object path, and the reporting endpoint for a deployment that
// puts its own chokepoint in front of the bucket and only wants the tap.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+g.pathPrefix+"{key...}", g.serveObject)

	// HEAD deliberately does not reinforce: asking for an object's metadata is not reading it, and
	// a client that HEADs before every GET would otherwise double every recall it makes.
	mux.HandleFunc("HEAD "+g.pathPrefix+"{key...}", g.serveObject)

	mux.HandleFunc("POST /recall", g.serveRecall)

	return mux
}

// serveObject answers one object read.
func (g *Gateway) serveObject(w http.ResponseWriter, r *http.Request) {
	log.Trace("func() gateway.serveObject")

	started := time.Now()

	if !g.authorised(r) {
		g.record(r.Context(), OutcomeUnauthorise, started)
		unauthorised(w)

		return
	}

	key := r.PathValue("key")
	if key == "" {
		g.record(r.Context(), OutcomeNotFound, started)
		http.Error(w, "no object key", http.StatusNotFound)

		return
	}

	// Reinforcement first, and only for a GET: by the time the object has been streamed the
	// response is already committed, so a failure there could not be reported anyway. It is
	// deliberately not conditional on the read succeeding - a read of an object the bucket has
	// already lost is still evidence that somebody wanted it.
	if r.Method == http.MethodGet {
		g.reinforce(r, key)
	}

	if g.mode == ModeRedirect {
		g.redirect(w, r, key, started)

		return
	}

	g.proxy(w, r, key, started)
}

// redirect presigns the object and sends the reader to it.
func (g *Gateway) redirect(w http.ResponseWriter, r *http.Request, key string, started time.Time) {
	url, err := g.store.Presign(r.Context(), key, g.urlTTL)
	if err != nil {
		log.WithError(err).WithField("key", key).Error("failed to presign an object")

		g.record(r.Context(), OutcomeFailed, started)
		http.Error(w, "cannot presign the object", http.StatusBadGateway)

		return
	}

	// No-store rather than the default: the redirect carries a credential with a short life, and a
	// cache holding it would hand it to readers after it has expired and, worse, to readers the
	// gateway never authorised.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", url)
	w.WriteHeader(http.StatusFound)

	g.record(r.Context(), OutcomeRedirected, started)
}

// proxy streams the object through this process.
func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request, key string, started time.Time) {
	reader, err := g.store.Get(r.Context(), key, objects.GetOptions{Range: r.Header.Get("Range")})
	if err != nil {
		if errors.Is(err, objects.ErrNotFound) {
			g.record(r.Context(), OutcomeNotFound, started)
			http.Error(w, "no such object", http.StatusNotFound)

			return
		}

		log.WithError(err).WithField("key", key).Error("failed to read an object")

		g.record(r.Context(), OutcomeFailed, started)
		http.Error(w, "cannot read the object", http.StatusBadGateway)

		return
	}

	defer func() { _ = reader.Body.Close() }()

	writeObjectHeaders(w, reader)

	status := http.StatusOK
	if reader.ContentRange != "" {
		status = http.StatusPartialContent
	}

	w.WriteHeader(status)

	if r.Method == http.MethodHead {
		g.record(r.Context(), OutcomeServed, started)

		return
	}

	// A copy that fails part way through has already sent a 200, so there is nothing to report to
	// the client and the only useful thing left is to say so in the log and in the metric.
	if _, err := io.Copy(w, reader.Body); err != nil {
		log.WithError(err).WithField("key", key).Warn("failed to stream an object to the client")

		g.record(r.Context(), OutcomeFailed, started)

		return
	}

	g.record(r.Context(), OutcomeServed, started)
}

func writeObjectHeaders(w http.ResponseWriter, reader *objects.Reader) {
	if reader.ContentType != "" {
		w.Header().Set("Content-Type", reader.ContentType)
	}

	if reader.ContentLength > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", reader.ContentLength))
	}

	if reader.ContentRange != "" {
		w.Header().Set("Content-Range", reader.ContentRange)
	}

	if reader.ETag != "" {
		w.Header().Set("ETag", reader.ETag)
	}

	if !reader.LastModified.IsZero() {
		w.Header().Set("Last-Modified", reader.LastModified.UTC().Format(http.TimeFormat))
	}
}

// recallRequest is the body of POST /recall: the keys somebody else's chokepoint just served.
type recallRequest struct {
	Keys []string `json:"keys"`
}

// recallResponse reports what was accepted, so a caller wiring this up for the first time can tell
// that its keys are being understood without waiting for a metric to move.
type recallResponse struct {
	Accepted   int `json:"accepted"`
	Unmappable int `json:"unmappable"`
}

// serveRecall is the tap without the gateway: a deployment that already has a fetch proxy, a
// signed-URL issuer or an application that knows what it just served can POST the keys here and
// keep its own request path.
//
// It reports what it accepted rather than what it reinforced, and the distinction is the point: a
// key is accepted once it has been buffered, and whether the store still holds a memory for it is
// answered by the hit rate rather than per request. Telling a caller "that key reinforced nothing"
// would invite it to treat a forgotten object as an error, which is precisely backwards.
func (g *Gateway) serveRecall(w http.ResponseWriter, r *http.Request) {
	log.Trace("func() gateway.serveRecall")

	if !g.authorised(r) {
		unauthorised(w)

		return
	}

	var request recallRequest

	if err := json.NewDecoder(io.LimitReader(r.Body, recallBodyLimit)).Decode(&request); err != nil {
		http.Error(w, "cannot parse the request body", http.StatusBadRequest)

		return
	}

	if len(request.Keys) > maxRecallKeys {
		http.Error(w, fmt.Sprintf("at most %d keys per request", maxRecallKeys), http.StatusBadRequest)

		return
	}

	response := recallResponse{}

	for _, key := range request.Keys {
		id, err := keymap.MemoryId(g.store.Bucket(), key)
		if err != nil {
			response.Unmappable++

			continue
		}

		if err := g.tap.Touch(r.Context(), id); err != nil {
			log.WithError(err).Debug("reinforcement failed for a reported key")
		}

		response.Accepted++
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)

	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.WithError(err).Debug("failed to write the recall response")
	}
}

// reinforce derives the id for a key and hands it to the tap, absorbing everything: an unmappable
// key is an object this integration cannot manage, and a tap failure is reinforcement lost. Neither
// is the reader's problem.
func (g *Gateway) reinforce(r *http.Request, key string) {
	id, err := keymap.MemoryId(g.store.Bucket(), key)
	if err != nil {
		tel.requests.Add(r.Context(), 1, observability.WithGroup(
			attribute.String(attrMode, string(g.mode)),
			attribute.String(attrOutcome, OutcomeUnmappable),
		))

		log.WithError(err).WithField("key", key).
			Debug("read an object that cannot be mapped to a memory id")

		return
	}

	if err := g.tap.Touch(r.Context(), id); err != nil {
		log.WithError(err).WithField("id", id).
			Warn("failed to reinforce a memory for an object that was read")
	}
}

// authorised reports whether the request carries the configured bearer token. With no token
// configured every request is authorised, which the command only permits when it was asked for.
func (g *Gateway) authorised(r *http.Request) bool {
	if g.authToken == "" {
		return true
	}

	header := r.Header.Get("Authorization")

	token, found := strings.CutPrefix(header, "Bearer ")
	if !found {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(g.authToken)) == 1
}

func unauthorised(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorised", http.StatusUnauthorized)
}

func (g *Gateway) record(ctx context.Context, outcome string, started time.Time) {
	attrs := observability.WithGroup(
		attribute.String(attrMode, string(g.mode)),
		attribute.String(attrOutcome, outcome),
	)

	tel.requests.Add(ctx, 1, attrs)
	tel.duration.Record(ctx, time.Since(started).Seconds(), attrs)
}

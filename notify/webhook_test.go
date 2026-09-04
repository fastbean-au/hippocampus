package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sampleDelivery is a representative memory-forgotten batch, used wherever the content of the
// delivery is not what the test is about.
func sampleDelivery() Delivery {
	return Delivery{
		Kind:     KindMemoryForgotten,
		Cause:    CauseConsolidation,
		QueuedAt: 1700000000000000000,
		CycleId:  7,
		Items: []Item{
			{Id: "m1", EventId: "e1", Group: "svc-a", Significance: 3, Bytes: 11},
			{Id: "m2", Group: "svc-b", Significance: 1, Bytes: 4},
		},
	}
}

func TestNoopIsDisabled(t *testing.T) {
	n := NewNoop()

	if n.Enabled() {
		t.Error("the no-op notifier reports Enabled() true")
	}

	if err := n.Deliver(context.Background(), sampleDelivery()); err != ErrDisabled {
		t.Errorf("the no-op notifier returned %v, want ErrDisabled", err)
	}
}

// TestNewWebhookRejectsUnusableConfiguration pins that the constructor fails on configuration it
// could never work with, and only on that: an endpoint that is simply down must still start.
func TestNewWebhookRejectsUnusableConfiguration(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no url", Config{}},
		{"blank url", Config{URL: "   "}},
		{"unparseable", Config{URL: "://nope"}},
		{"wrong scheme", Config{URL: "ftp://example.com/hook"}},
		{"no host", Config{URL: "http:///hook"}},
		{"half a client certificate", Config{URL: "https://example.com/hook", TLS: TLSConfig{CertFile: "cert.pem"}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewWebhook(c.cfg); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}

	// The one that must NOT fail: a well-formed URL nothing is listening on.
	if _, err := NewWebhook(Config{URL: "http://127.0.0.1:1/hook"}); err != nil {
		t.Errorf("an unreachable endpoint must not fail construction: %s", err.Error())
	}
}

func TestWebhookDeliversTheBatch(t *testing.T) {
	var (
		gotPath   string
		gotMethod string
		gotKind   string
		gotAuth   string
		got       Delivery
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotKind = r.Header.Get(kindHeader)
		gotAuth = r.Header.Get("Authorization")

		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("decoding the delivery: %s", err.Error())
		}

		w.WriteHeader(http.StatusAccepted)
	}))

	defer srv.Close()

	hook, err := NewWebhook(Config{URL: srv.URL + "/hook", Token: "s3cret"})
	if err != nil {
		t.Fatalf("NewWebhook: %s", err.Error())
	}

	if err := hook.Deliver(context.Background(), sampleDelivery()); err != nil {
		t.Fatalf("Deliver: %s", err.Error())
	}

	if gotMethod != http.MethodPost || gotPath != "/hook" {
		t.Errorf("delivered %s %s, want POST /hook", gotMethod, gotPath)
	}

	if gotKind != string(KindMemoryForgotten) {
		t.Errorf("kind header was %q", gotKind)
	}

	if gotAuth != "Bearer s3cret" {
		t.Errorf("authorization header was %q", gotAuth)
	}

	if len(got.Items) != 2 || got.Items[0].Id != "m1" || got.Items[1].Group != "svc-b" {
		t.Errorf("the receiver saw %+v", got.Items)
	}

	if got.Cause != CauseConsolidation || got.CycleId != 7 {
		t.Errorf("cause/cycle not carried: %+v", got)
	}
}

// TestWebhookSignsTheBody verifies the signature covers the timestamp AND the body, which is what
// stops a captured delivery being replayed with a fresh timestamp.
func TestWebhookSignsTheBody(t *testing.T) {
	const secret = "a-secret-at-least-thirty-two-bytes-long"

	var (
		gotSignature string
		gotTimestamp string
		gotBody      []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature = r.Header.Get(signatureHeader)
		gotTimestamp = r.Header.Get(timestampHeader)
		gotBody, _ = io.ReadAll(r.Body)
	}))

	defer srv.Close()

	hook, err := NewWebhook(Config{URL: srv.URL, SigningSecret: secret})
	if err != nil {
		t.Fatalf("NewWebhook: %s", err.Error())
	}

	if err := hook.Deliver(context.Background(), sampleDelivery()); err != nil {
		t.Fatalf("Deliver: %s", err.Error())
	}

	if _, err := strconv.ParseInt(gotTimestamp, 10, 64); err != nil {
		t.Fatalf("timestamp header %q is not a UnixNano: %s", gotTimestamp, err.Error())
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(gotTimestamp))
	mac.Write([]byte("."))
	mac.Write(gotBody)

	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if gotSignature != want {
		t.Errorf("signature was %q, want %q", gotSignature, want)
	}

	// The timestamp is inside the signature: verifying the body alone must not reproduce it.
	bare := hmac.New(sha256.New, []byte(secret))
	bare.Write(gotBody)

	if gotSignature == "sha256="+hex.EncodeToString(bare.Sum(nil)) {
		t.Error("the signature covers the body alone - a captured delivery could be replayed with a fresh timestamp")
	}
}

// TestWebhookSignsNothingWithoutASecret pins that the headers are absent rather than empty, so a
// receiver can tell "unsigned" from "signed with the empty key".
func TestWebhookSignsNothingWithoutASecret(t *testing.T) {
	var present bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, signed := r.Header[http.CanonicalHeaderKey(signatureHeader)]
		_, stamped := r.Header[http.CanonicalHeaderKey(timestampHeader)]
		present = signed || stamped
	}))

	defer srv.Close()

	hook, _ := NewWebhook(Config{URL: srv.URL})

	if err := hook.Deliver(context.Background(), sampleDelivery()); err != nil {
		t.Fatalf("Deliver: %s", err.Error())
	}

	if present {
		t.Error("signature headers were sent without a signing secret")
	}
}

// TestWebhookTreatsEveryNon2xxAsAFailure is the property the drain worker depends on: a delivery
// the receiver did not accept must come back as an error, or the queue confirms and loses it.
func TestWebhookTreatsEveryNon2xxAsAFailure(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("the reason"))
			}))

			defer srv.Close()

			hook, _ := NewWebhook(Config{URL: srv.URL})

			err := hook.Deliver(context.Background(), sampleDelivery())
			if err == nil {
				t.Fatalf("status %d was reported as a success", status)
			}

			if !strings.Contains(err.Error(), strconv.Itoa(status)) || !strings.Contains(err.Error(), "the reason") {
				t.Errorf("the error names neither the status nor the body: %s", err.Error())
			}
		})
	}
}

// TestWebhookDoesNotFollowRedirects pins that a bearer token and a signature are never forwarded to
// whatever host the endpoint names.
func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	var elsewhereCalled bool

	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereCalled = true
	}))

	defer elsewhere.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))

	defer srv.Close()

	hook, _ := NewWebhook(Config{URL: srv.URL, Token: "s3cret"})

	if err := hook.Deliver(context.Background(), sampleDelivery()); err == nil {
		t.Error("a redirect was reported as a successful delivery")
	}

	if elsewhereCalled {
		t.Error("the redirect was followed - the bearer token was forwarded to another host")
	}
}

// TestWebhookRespectsTheContext covers both halves: a cancelled caller stops the attempt, and the
// configured timeout bounds a receiver that never answers.
func TestWebhookRespectsTheContext(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))

	defer func() {
		close(release)
		srv.Close()
	}()

	hook, _ := NewWebhook(Config{URL: srv.URL, Timeout: 100 * time.Millisecond})

	started := time.Now()

	if err := hook.Deliver(context.Background(), sampleDelivery()); err == nil {
		t.Error("a hung receiver was reported as a successful delivery")
	}

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the timeout did not bound the attempt: took %s", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := hook.Deliver(ctx, sampleDelivery()); err == nil {
		t.Error("a cancelled context was reported as a successful delivery")
	}
}

// TestWebhookCarriesTheSleepCycle pins that the completion shape round-trips - the chunk numbering
// in particular, which is how a receiver tells a partial view from a complete one.
func TestWebhookCarriesTheSleepCycle(t *testing.T) {
	var got Delivery

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
	}))

	defer srv.Close()

	hook, _ := NewWebhook(Config{URL: srv.URL})

	sent := Delivery{
		Kind:    KindSleepCompleted,
		CycleId: 42,
		Chunk:   2,
		Chunks:  3,
		Items:   []Item{{Id: "m9"}},
		Cycle: &Cycle{
			Trigger:              "timer",
			MemoriesConsolidated: 1200,
			BytesFreed:           98765,
			Success:              true,
		},
	}

	if err := hook.Deliver(context.Background(), sent); err != nil {
		t.Fatalf("Deliver: %s", err.Error())
	}

	if got.Chunk != 2 || got.Chunks != 3 || got.CycleId != 42 {
		t.Errorf("chunking not carried: %+v", got)
	}

	if got.Cycle == nil || got.Cycle.MemoriesConsolidated != 1200 || !got.Cycle.Success {
		t.Errorf("the cycle summary did not round-trip: %+v", got.Cycle)
	}
}

// TestWebhookOmitsRatherThanTruncatesABody pins the wire contract for an oversized body: the flag
// travels and the field is empty, so a receiver is never handed part of a body believing it whole.
func TestWebhookOmitsRatherThanTruncatesABody(t *testing.T) {
	var got Delivery

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
	}))

	defer srv.Close()

	hook, _ := NewWebhook(Config{URL: srv.URL})

	sent := sampleDelivery()
	sent.Items[0].Body = "kept"
	sent.Items[1].BodyOmitted = true

	if err := hook.Deliver(context.Background(), sent); err != nil {
		t.Fatalf("Deliver: %s", err.Error())
	}

	if got.Items[0].Body != "kept" || got.Items[0].BodyOmitted {
		t.Errorf("a body that fitted was not carried: %+v", got.Items[0])
	}

	if got.Items[1].Body != "" || !got.Items[1].BodyOmitted {
		t.Errorf("an oversized body must arrive empty and flagged: %+v", got.Items[1])
	}
}

// TestWebhookEnabledAndEndpoint covers the two accessors, including the nil receiver: the topology
// view reaches Endpoint through a Notifier that may be the no-op, and a nil dereference there would
// take down a page rather than show an empty node.
func TestWebhookEnabledAndEndpoint(t *testing.T) {
	hook, err := NewWebhook(Config{URL: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("NewWebhook: %s", err.Error())
	}

	if !hook.Enabled() {
		t.Error("a configured webhook reports Enabled() false")
	}

	if hook.Endpoint() != "https://example.com/hook" {
		t.Errorf("Endpoint() = %q", hook.Endpoint())
	}

	var absent *Webhook

	if absent.Enabled() {
		t.Error("a nil webhook reports Enabled() true")
	}

	if absent.Endpoint() != "" {
		t.Errorf("a nil webhook reported an endpoint: %q", absent.Endpoint())
	}
}

// TestWebhookSanitisesInvalidUTF8 pins what actually happens to a body that is not valid UTF-8:
// encoding/json replaces the bytes rather than refusing them, so the delivery still lands.
//
// It cannot arise from the store - a memory body is a proto3 string and so is valid UTF-8 by the
// time it is written - but it is worth knowing which way this falls, because the alternative would
// be a delivery that fails to marshal and is retried forever, which no redelivery can fix. (That is
// the poison-message trap the Bluesky bridge hit from the other direction, in
// integrations/eventsource.)
func TestWebhookSanitisesInvalidUTF8(t *testing.T) {
	var reached bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	defer srv.Close()

	hook, _ := NewWebhook(Config{URL: srv.URL})

	delivery := sampleDelivery()
	delivery.Items[0].Body = string([]byte{0xff, 0xfe, 0xfd})

	if err := hook.Deliver(context.Background(), delivery); err != nil {
		t.Errorf("an invalid-UTF-8 body failed to deliver, which no retry could fix: %s", err.Error())
	}

	if !reached {
		t.Error("the delivery never reached the receiver")
	}
}

// TestWebhookRejectsAnUnbuildableRequest covers the request-construction failure, reached by a URL
// that parses at construction and is rejected by http.NewRequest.
func TestWebhookRejectsAnUnbuildableRequest(t *testing.T) {
	hook, err := NewWebhook(Config{URL: "http://example.com/hook"})
	if err != nil {
		t.Fatalf("NewWebhook: %s", err.Error())
	}

	// A control character in the URL is refused by the request builder, not by url.Parse.
	hook.url = "http://example.com/\x7f"

	if err := hook.Deliver(context.Background(), sampleDelivery()); err == nil {
		t.Error("an unbuildable request was reported as sent")
	}
}

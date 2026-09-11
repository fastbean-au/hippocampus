package reap

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/notify"

	"github.com/fastbean-au/hippocampus/integrations/objectstore/objects"
)

func newReceiver(t *testing.T, cfg ReceiverConfig) (*Receiver, *objects.Memory) {
	t.Helper()

	store := objects.NewMemory("payloads")
	store.Put("traces/one.json", []byte("one"), time.Now())

	reaper, err := New(Config{Store: store, Delete: true})
	if err != nil {
		t.Fatalf("New failed: %s", err.Error())
	}

	cfg.Reaper = reaper

	receiver, err := NewReceiver(cfg)
	if err != nil {
		t.Fatalf("NewReceiver failed: %s", err.Error())
	}

	return receiver, store
}

func delivery(kind notify.Kind, cause notify.Cause, ids ...string) []byte {
	items := make([]notify.Item, 0, len(ids))

	for _, v := range ids {
		items = append(items, notify.Item{Id: v})
	}

	body, _ := json.Marshal(notify.Delivery{
		Kind:     kind,
		Cause:    cause,
		QueuedAt: time.Now().UnixNano(),
		Items:    items,
	})

	return body
}

func post(t *testing.T, receiver *Receiver, body []byte, decorate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/callbacks", bytesReader(body))

	if decorate != nil {
		decorate(request)
	}

	recorder := httptest.NewRecorder()
	receiver.ServeHTTP(recorder, request)

	return recorder
}

func TestADeliveryDeletesTheObjectsBehindIt(t *testing.T) {
	receiver, store := newReceiver(t, ReceiverConfig{})

	recorder := post(t, receiver, delivery(notify.KindMemoryForgotten, notify.CauseConsolidation, "payloads/traces/one.json"), nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}

	if store.Has("traces/one.json") {
		t.Error("expected the object to have been deleted")
	}
}

// The status codes are a contract with the service's drain worker: a 2xx means "forget it", so a
// delivery this process could not carry out must be a 5xx or the instruction is lost silently.
func TestAFailedDeletionAsksForARetry(t *testing.T) {
	receiver, store := newReceiver(t, ReceiverConfig{})
	store.DeleteErr = fmt.Errorf("access denied")

	recorder := post(t, receiver, delivery(notify.KindMemoryForgotten, notify.CauseConsolidation, "payloads/traces/one.json"), nil)

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 so the queue replays the delivery, got %d", recorder.Code)
	}
}

// And the converse: a kind or a cause this agent deliberately does not act on must be a 2xx, or the
// queue would replay it forever.
func TestADeliveryItDoesNotActOnIsStillAccepted(t *testing.T) {
	receiver, store := newReceiver(t, ReceiverConfig{})

	cases := []struct {
		name string
		body []byte
	}{
		{name: "another kind", body: delivery(notify.KindEventForgotten, notify.CauseConsolidation, "payloads/traces/one.json")},
		{name: "a cause outside the default set", body: delivery(notify.KindMemoryForgotten, notify.CauseClear, "payloads/traces/one.json")},
		{name: "a purge", body: delivery(notify.KindMemoryForgotten, notify.CausePurge, "payloads/traces/one.json")},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			recorder := post(t, receiver, v.body, nil)

			if recorder.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", recorder.Code)
			}

			if !store.Has("traces/one.json") {
				t.Error("expected the object to survive")
			}
		})
	}
}

func TestTheBearerTokenIsEnforced(t *testing.T) {
	receiver, _ := newReceiver(t, ReceiverConfig{Token: "sekrit"})

	body := delivery(notify.KindMemoryForgotten, notify.CauseConsolidation, "payloads/traces/one.json")

	recorder := post(t, receiver, body, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without the token, got %d", recorder.Code)
	}

	recorder = post(t, receiver, body, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer sekrit")
	})

	if recorder.Code != http.StatusOK {
		t.Errorf("expected 200 with the token, got %d", recorder.Code)
	}
}

// The signature is reproduced exactly as notify.Webhook computes it. A token proves who is calling;
// this proves the body - the list of objects to delete - was not altered in between.
func TestTheSignatureIsVerified(t *testing.T) {
	const secret = "shared"

	receiver, _ := newReceiver(t, ReceiverConfig{Secret: secret})

	body := delivery(notify.KindMemoryForgotten, notify.CauseConsolidation, "payloads/traces/one.json")
	timestamp := strconv.FormatInt(time.Now().UnixNano(), 10)

	signed := func(r *http.Request) {
		r.Header.Set(timestampHeader, timestamp)
		r.Header.Set(signatureHeader, "sha256="+hex.EncodeToString(sign([]byte(secret), timestamp, body)))
	}

	if recorder := post(t, receiver, body, signed); recorder.Code != http.StatusOK {
		t.Errorf("expected a correctly signed delivery to be accepted, got %d", recorder.Code)
	}

	cases := []struct {
		name      string
		decorate  func(*http.Request)
		bodySent  []byte
		expecting int
	}{
		{
			name:      "no signature at all",
			decorate:  nil,
			bodySent:  body,
			expecting: http.StatusUnauthorized,
		},
		{
			name: "a signature over a different body",
			decorate: func(r *http.Request) {
				r.Header.Set(timestampHeader, timestamp)
				r.Header.Set(signatureHeader, "sha256="+hex.EncodeToString(sign([]byte(secret), timestamp, []byte("{}"))))
			},
			bodySent:  body,
			expecting: http.StatusUnauthorized,
		},
		{
			name: "a signature that is not hexadecimal",
			decorate: func(r *http.Request) {
				r.Header.Set(timestampHeader, timestamp)
				r.Header.Set(signatureHeader, "sha256=not-hex")
			},
			bodySent:  body,
			expecting: http.StatusUnauthorized,
		},
		{
			name: "no timestamp",
			decorate: func(r *http.Request) {
				r.Header.Set(signatureHeader, "sha256="+hex.EncodeToString(sign([]byte(secret), timestamp, body)))
			},
			bodySent:  body,
			expecting: http.StatusUnauthorized,
		},
		{
			name: "an unparseable timestamp",
			decorate: func(r *http.Request) {
				r.Header.Set(timestampHeader, "yesterday")
				r.Header.Set(signatureHeader, "sha256="+hex.EncodeToString(sign([]byte(secret), "yesterday", body)))
			},
			bodySent:  body,
			expecting: http.StatusUnauthorized,
		},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			if recorder := post(t, receiver, v.bodySent, v.decorate); recorder.Code != v.expecting {
				t.Errorf("expected %d, got %d", v.expecting, recorder.Code)
			}
		})
	}
}

// The replay window is two-sided: a delivery from the future is as suspect as an old one, and a
// receiver whose clock has drifted should say so rather than accept anything.
func TestTheReplayWindowIsTwoSided(t *testing.T) {
	const secret = "shared"

	receiver, _ := newReceiver(t, ReceiverConfig{Secret: secret, MaxAge: time.Minute})

	body := delivery(notify.KindMemoryForgotten, notify.CauseConsolidation, "payloads/traces/one.json")

	for _, offset := range []time.Duration{-time.Hour, time.Hour} {
		timestamp := strconv.FormatInt(time.Now().Add(offset).UnixNano(), 10)

		recorder := post(t, receiver, body, func(r *http.Request) {
			r.Header.Set(timestampHeader, timestamp)
			r.Header.Set(signatureHeader, "sha256="+hex.EncodeToString(sign([]byte(secret), timestamp, body)))
		})

		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("expected a delivery %s outside the window to be refused, got %d", offset, recorder.Code)
		}
	}
}

func TestRubbishIsRejected(t *testing.T) {
	receiver, _ := newReceiver(t, ReceiverConfig{})

	recorder := post(t, receiver, []byte("{"), nil)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", recorder.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/callbacks", nil)
	get := httptest.NewRecorder()

	receiver.ServeHTTP(get, request)

	if get.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for a GET, got %d", get.Code)
	}
}

// A stalled cycle is the store saying it has stopped forgetting because THIS receiver was not
// accepting deliveries, so the delivery is accepted and the fact is logged rather than acted on.
func TestAStalledCycleIsAccepted(t *testing.T) {
	receiver, _ := newReceiver(t, ReceiverConfig{})

	body, _ := json.Marshal(notify.Delivery{
		Kind:     notify.KindSleepCompleted,
		QueuedAt: time.Now().UnixNano(),
		Cycle: &notify.Cycle{
			Stalled:       true,
			StalledReason: "the callback queue is over its row cap",
		},
	})

	if recorder := post(t, receiver, body, nil); recorder.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", recorder.Code)
	}
}

// The kind reaches this process on the wire, from a caller that may not have authenticated yet, so
// it can never be used as a metric attribute unclamped.
func TestTheKindAttributeIsClamped(t *testing.T) {
	if got := knownKind("memory_forgotten"); got != "memory_forgotten" {
		t.Errorf("expected a known kind to pass through, got %q", got)
	}

	for _, v := range []string{"", "whatever-i-like", "memory_forgotten "} {
		if got := knownKind(v); got != "unknown" {
			t.Errorf("expected %q to be clamped, got %q", v, got)
		}
	}
}

func TestNewReceiverRequiresAReaper(t *testing.T) {
	if _, err := NewReceiver(ReceiverConfig{}); err == nil {
		t.Error("expected a receiver with no reaper to be refused")
	}
}

func bytesReader(body []byte) io.Reader {
	return bytes.NewReader(body)
}

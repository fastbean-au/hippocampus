package bluesky

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestJetstreamLive connects to a real Jetstream endpoint and checks that what arrives still decodes
// and still maps onto a bridge.Message.
//
// It skips unless HIPPOCAMPUS_TEST_JETSTREAM names an endpoint. Unlike the MQTT and RabbitMQ
// integration tests this needs NO container - Jetstream is a public, unauthenticated service - so it
// costs one environment variable and no service definition. It deliberately stores nothing: what it
// covers is the real dial and the real wire, which the fakes cannot, and nothing else.
//
// The wire it asserts on is frozen (Jetstream v2 keeps /subscribe's v1 payload for existing
// consumers), so a failure here means either the endpoint has changed or the freeze has ended - both
// worth knowing about, and neither detectable from a fixture.
func TestJetstreamLive(t *testing.T) {
	endpoint := os.Getenv("HIPPOCAMPUS_TEST_JETSTREAM")
	if endpoint == "" {
		t.Skip("HIPPOCAMPUS_TEST_JETSTREAM is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := defaultDial(ctx, Config{
		URL:         endpoint,
		Collections: []string{CollectionPost, CollectionLike},
		ReadTimeout: 10 * time.Second,
	}, 0)
	if err != nil {
		t.Fatalf("dialling %s: %s", endpoint, err)
	}

	defer func() { _ = s.Close() }()

	var (
		posts int
		likes int
	)

	// The public firehose delivers tens of posts a second, so a handful of frames is a moment's wait;
	// the context bounds it in case the endpoint is up but idle.
	for posts == 0 || likes == 0 {
		if ctx.Err() != nil {
			t.Fatalf("saw %d posts and %d likes before the deadline", posts, likes)
		}

		frame, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("reading a frame: %s", err)
		}

		var ev event

		if err := json.Unmarshal(frame, &ev); err != nil {
			t.Fatalf("decoding a live frame: %s\nframe: %s", err, truncate(string(frame), 512))
		}

		if ev.Kind != kindCommit || ev.Commit == nil || ev.Commit.Operation != operationCreate {
			continue
		}

		if ev.cursor() == 0 {
			t.Error("a live frame carried neither a cursor nor a time_us, so a reconnect could not resume")
		}

		msg := toMessage(&ev)

		switch ev.Commit.Collection {

		case CollectionPost:
			if ev.Commit.Record == nil || ev.Commit.Record.Text == "" {
				continue
			}

			posts++

			if msg.Headers[hdrURI] == "" {
				t.Error("a live post produced no at:// URI, so it would have no memory id")
			}

			if string(msg.Data) != ev.Commit.Record.Text {
				t.Errorf("body = %q, want the post text", msg.Data)
			}

		case CollectionLike:
			if ev.Commit.Record == nil || ev.Commit.Record.Subject == nil {
				continue
			}

			likes++

			// This is the assertion the whole adapter rests on: a like names its target with the same
			// at:// URI that a post's memory is keyed on.
			if got := msg.Headers[hdrSubject]; got == "" {
				t.Error("a live like carried no subject URI, so it could reinforce nothing")
			}
		}
	}

	t.Logf("decoded %d posts and %d likes from %s", posts, likes, endpoint)
}

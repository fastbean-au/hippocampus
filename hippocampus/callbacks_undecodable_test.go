package hippocampus

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/fastbean-au/hippocampus/db"
)

// corruptingStore marks one queued row as undecodable, as ClaimCallbacks reports a payload it
// cannot read. The corruption itself is pinned in the db package; this is what the dispatcher does
// with it.
type corruptingStore struct {
	db.Store
	bad int64
}

func (c corruptingStore) ClaimCallbacks(ctx context.Context, limit int, now int64) ([]db.CallbackDelivery, error) {
	claimed, err := c.Store.ClaimCallbacks(ctx, limit, now)

	for i := range claimed {
		if claimed[i].Seq == c.bad {
			claimed[i].Payload = db.CallbackPayload{}
			claimed[i].DecodeErr = errors.New("decoding a callback payload: invalid character")
		}
	}

	return claimed, err
}

// TestAnUndecodableDeliveryIsAbandonedNotRetriedForever (TODO-3 item 162): under a retaining policy
// the caps never remove a memory_forgotten row, so a corrupt one at the head of the queue would be
// claimed on every pass forever and, with a batch of them, nothing behind it would ever be sent.
//
// Not parallel: it replaces a package variable (tel).
func TestAnUndecodableDeliveryIsAbandonedNotRetriedForever(t *testing.T) {
	restoreProvider := otel.GetMeterProvider()
	restoreTel := tel

	t.Cleanup(func() {
		otel.SetMeterProvider(restoreProvider)
		tel = restoreTel
	})

	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	tel = newTelemetry()

	s, database := stallServer(t, BacklogRetain, db.QueueBounds{MaxRows: 100})
	s.callbackBatchSize = 1

	backlogUp(t, database, 2, time.Now().UnixNano())

	queued, err := database.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil || len(queued) != 2 {
		t.Fatalf("seeding: claimed %d, %v", len(queued), err)
	}

	s.db = corruptingStore{Store: database, bad: queued[0].Seq}

	notifier := &recordingNotifier{}

	// Pass one meets the corrupt row; pass two must reach the good one behind it.
	s.dispatchCallbacksOnce(notifier)
	s.dispatchCallbacksOnce(notifier)

	if sent := notifier.taken(); len(sent) != 1 {
		t.Fatalf("delivered %d, want the one good delivery behind the corrupt row", len(sent))
	}

	depth, err := database.CallbackQueueDepth(context.Background())
	if err != nil {
		t.Fatalf("CallbackQueueDepth: %s", err)
	}

	if depth != 0 {
		t.Errorf("%d rows remain queued, want the corrupt row removed rather than claimed again", depth)
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %s", err)
	}

	var abandoned int64

	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "hippocampus.callbacks.abandoned" {
				continue
			}

			for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
				abandoned += point.Value
			}
		}
	}

	if abandoned != 1 {
		t.Errorf("hippocampus.callbacks.abandoned = %d, want the corrupt delivery counted", abandoned)
	}
}

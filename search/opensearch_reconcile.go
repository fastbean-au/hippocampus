package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	log "github.com/sirupsen/logrus"
)

// DeleteMemoriesSync removes documents synchronously, bypassing the asynchronous queue and
// returning the error.
//
// It exists for the delete outbox's drain (db/outbox.go). The whole point of the outbox is that a
// delete cannot be lost, so the drain must not hand the work back to the queue that loses it: the
// queue drops on overflow at the enqueue boundary, which is exactly the failure the outbox is
// there to prevent. The drain needs to know whether the deletion landed before it removes the
// row, so this reports it.
//
// The service's own write path keeps using DeleteMemories (asynchronous, never blocking,
// FIFO-ordered against indexes), which is now backed by the outbox rather than relied upon.
func (o *OpenSearch) DeleteMemoriesSync(ctx context.Context, ids []string) error {
	log.Trace("func() search.DeleteMemoriesSync")

	if len(ids) == 0 {

		return nil
	}

	// applyTimeout bounds ONE round trip, so the budget has to scale with how many the batch will
	// actually take. Giving a five-hundred-id batch a single operation's deadline is what made the
	// stale sweep abandon and restart from the top of the index rather than converge, back when a
	// batch was N sequential deletes; deleteIds is one request per chunk now, but the deadline still
	// has to cover every chunk or a large drain reintroduces the same failure.
	chunks := (len(ids) + bulkDeleteChunk - 1) / bulkDeleteChunk

	ctx, cancel := context.WithTimeout(ctx, time.Duration(chunks)*o.applyTimeout)
	defer cancel()

	if !o.indexReady.Load() {
		if err := o.ensureIndex(ctx); err != nil {

			return err
		}
	}

	return o.apply(ctx, op{kind: opDeleteIds, ids: ids})
}

// IndexCursor is a position in the index, held by the caller across the pages of an enumeration.
//
// It is a timestamp plus an offset within it rather than a document id, and that is the load-bearing
// decision here. Sorting on _id is the obvious way to page an index exhaustively, and it works - but
// _id has no doc values, so sorting on it loads the field into heap as fielddata: measured at 26
// bytes per document, which is ~116 MB on the index that motivated this change, and grows linearly
// with it. A housekeeping sweep must not cost more heap the more there is to sweep. The mapped
// timestamp field has doc values and sorts for nothing.
//
// The price is that a timestamp is not unique, so it cannot be a search_after key on its own:
// documents sharing the boundary value would be skipped, silently, which is the one thing this sweep
// exists not to do. Offset closes that - the query is an INCLUSIVE range and Offset is how many
// documents at exactly that timestamp have already been handled, which is what `from` skips, since
// ascending order puts them first. It is bounded by how many documents share one instant rather than
// by the size of the index, so this is not deep pagination wearing a different name. Usually it is
// zero: see IndexPage.
type IndexCursor struct {
	Timestamp int64
	Offset    int
}

// IndexPage is one page of an enumeration.
//
// The subtlety is Partial, and it exists because THE CALLER DELETES WHAT IT ENUMERATES. There is no
// snapshot behind this - each request is independent - so a document removed from an earlier page
// shifts every later document down, and an offset computed before the removal would step over
// whatever moved into the gap. That is the same silent skip the cursor design set out to avoid,
// arriving from the other direction.
//
// So a page normally ends on a timestamp BOUNDARY: the trailing group of documents sharing the final
// timestamp is held back for the next page, and the cursor moves to the next instant with a zero
// offset. Nothing the caller deletes can then affect where the next page starts, because everything
// it deleted sorts strictly before it.
//
// Partial is the one case that cannot be aligned: every document in the page shared a single
// timestamp, so there is no boundary inside it to stop at. Then, and only then, the offset is in
// play, and the caller must reduce Next.Offset by however many of these ids it removed - which it
// knows, and this package cannot.
type IndexPage struct {
	Ids     []string
	Next    IndexCursor
	Done    bool
	Partial bool
}

// enumerateDefaultSize is the page size used when a caller asks for none.
const enumerateDefaultSize = 500

// EnumerateIdsPage reads one page of document ids, in timestamp order, resuming from cursor. The
// zero cursor starts at the beginning; a Done page reports that the index is exhausted.
//
// It backs the reconciliation sweep's reverse direction - finding documents whose memory the primary
// store no longer holds. Each request is independent (no scroll, no point-in-time), so a sweep can
// stop at shutdown and the next one resumes or restarts without leaving server-side state behind.
//
// Because there is no snapshot, a page reflects the index as it is at that moment. That is correct
// for this caller rather than merely tolerable: it acts only on documents the primary store says are
// gone, and a document written mid-sweep is one the store has.
func (o *OpenSearch) EnumerateIdsPage(ctx context.Context, cursor IndexCursor, size int) (IndexPage, error) {
	log.Trace("func() search.EnumerateIdsPage")

	if size <= 0 {
		size = enumerateDefaultSize
	}

	ctx, cancel := context.WithTimeout(ctx, o.applyTimeout)
	defer cancel()

	if !o.indexReady.Load() {
		if err := o.ensureIndex(ctx); err != nil {

			return IndexPage{}, err
		}
	}

	// _source false: only the ids are wanted, and a page of bodies would be orders of magnitude
	// larger than the question deserves.
	body := map[string]any{
		"size":    size,
		"from":    cursor.Offset,
		"_source": false,
		"sort":    []any{map[string]any{"timestamp": "asc"}},

		// NOT an optimisation, and not optional: without it this walk silently skips documents.
		//
		// Sorting on a numeric field lets OpenSearch early-terminate collection using the field's
		// point index, and the page it then returns is NOT the true lowest-N - it is a
		// non-exhaustive sample spread across the range. The cursor advances past the highest
		// timestamp it saw, so everything the optimisation skipped is skipped by the sweep too, and
		// nothing reports it. Measured on a 2M-document index: one page of 500 spanned a window
		// holding 1,472,040 documents, and a full walk terminated after 208,023 of 2,086,990.
		//
		// track_total_hits: true disables that optimisation, which is the documented control for it.
		// Wrapping the range in a bool.filter also happened to avoid it, but that is optimiser
		// behaviour rather than a contract, and this walk's correctness cannot rest on it. The cost
		// is real (~66ms against ~8ms per page on that index) and irrelevant here, since the sweep
		// paces itself at reconcilePageDelay between pages anyway.
		"track_total_hits": true,
		"query": map[string]any{
			"range": map[string]any{
				"timestamp": map[string]any{"gte": cursor.Timestamp},
			},
		},

		// The timestamp is asked for as a doc-value field rather than read off the sort value,
		// because a sort value arrives through an `any` and so has been through float64 - 53 bits of
		// mantissa against a UnixNano's 61, which collapses adjacent nanoseconds and, worse, rounds
		// UP. A cursor rounded up steps over whatever sits in the gap. The fields block comes back as
		// raw JSON, which this package decodes itself, exactly.
		"docvalue_fields": []any{"timestamp"},
	}

	payload, err := json.Marshal(body)
	if err != nil {

		return IndexPage{}, err
	}

	resp, err := o.client.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{o.index},
		Body:    strings.NewReader(string(payload)),
	})
	if err != nil {

		return IndexPage{}, fmt.Errorf("enumerating index ids: %w", err)
	}

	if resp == nil || len(resp.Hits.Hits) == 0 {

		return IndexPage{Done: true}, nil
	}

	return buildIndexPage(cursor, resp.Hits.Hits, size)
}

// buildIndexPage turns the hits into a page aligned to a timestamp boundary where it can be, and
// says so when it cannot. See IndexPage for why the alignment is the point.
func buildIndexPage(cursor IndexCursor, hits []opensearchapi.SearchHit, size int) (IndexPage, error) {
	timestamps := make([]int64, len(hits))

	for i, hit := range hits {
		ts, err := hitTimestamp(hit)
		if err != nil {

			return IndexPage{}, err
		}

		timestamps[i] = ts
	}

	last := timestamps[len(timestamps)-1]

	// A short page means the query exhausted what matched, so there is no trailing group still to
	// come and nothing to hold back - every document at `last` is already here.
	keep := len(hits)

	if len(hits) == size {
		for keep > 0 && timestamps[keep-1] == last {
			keep--
		}
	}

	// Every document in a full page shared one timestamp: there is no boundary inside it to stop at,
	// so the offset is in play and the caller has to account for its own deletions.
	if keep == 0 {
		// The offset carries over only if this page is still inside the instant the cursor named.
		// A page can be wholly one timestamp and yet be a LATER one - the cursor's own instant
		// having been exhausted by its offset - and there the count starts again from this page,
		// because the offset belonged to a timestamp the query has now moved past.
		offset := len(hits)

		if last == cursor.Timestamp {
			offset += cursor.Offset
		}

		return IndexPage{
			Ids:     hitIds(hits),
			Next:    IndexCursor{Timestamp: last, Offset: offset},
			Partial: true,
		}, nil
	}

	return IndexPage{
		Ids: hitIds(hits[:keep]),

		// The next instant, with a clean offset: everything this page carried sorts strictly before
		// it, so whatever the caller deletes cannot move the boundary.
		Next: IndexCursor{Timestamp: timestamps[keep-1] + 1},
	}, nil
}

// hitIds pulls the document ids out of a run of hits.
func hitIds(hits []opensearchapi.SearchHit) []string {
	out := make([]string, 0, len(hits))

	for _, hit := range hits {
		out = append(out, hit.ID)
	}

	return out
}

// hitTimestamp reads a hit's timestamp back exactly.
//
// From the doc-value field rather than hit.Sort, and decoded here rather than by the SDK: the SDK
// unmarshals sort values into []any, which turns every JSON number into a float64. A UnixNano
// timestamp does not survive that (see the request above), and a cursor built from a rounded
// timestamp would step over documents rather than merely revisit them.
func hitTimestamp(hit opensearchapi.SearchHit) (int64, error) {
	if len(hit.Fields) == 0 {

		return 0, fmt.Errorf("document '%s' came back with no timestamp field; it predates the mapping the sweep needs", hit.ID)
	}

	var fields struct {
		Timestamp []json.Number `json:"timestamp"`
	}

	decoder := json.NewDecoder(bytes.NewReader(hit.Fields))
	decoder.UseNumber()

	if err := decoder.Decode(&fields); err != nil {

		return 0, fmt.Errorf("reading the timestamp of document '%s': %w", hit.ID, err)
	}

	if len(fields.Timestamp) == 0 {

		return 0, fmt.Errorf("document '%s' has no timestamp value", hit.ID)
	}

	return fields.Timestamp[0].Int64()
}

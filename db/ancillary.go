package db

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// The store spends bytes its capacity target cannot see.
//
// Three tables are excluded from UsedBytes deliberately and correctly - the forgotten log, the
// search outbox and the callback queue (see tombstoneBytes, searchOutboxBytes and
// callbackQueueBytes for why each one has to be). The exclusion is right and is not in question
// here. What is in question is that it is TOTAL: nothing above the storage layer could ask how
// large the excluded part had grown, so on the embedded deployment - where the queues and the store
// are the same file - a receiver that was down while a large cycle ran grew that file, capacity
// pressure stayed flat, and the one number an operator is told to watch went on reporting headroom
// right up to a full disk.
//
// So the same figure the exclusion subtracts is also reported. That it is the SAME figure is the
// point of this file: tombstoneBytes, searchOutboxBytes and callbackQueueBytes now delegate here,
// so what the console shows and what the controller ignores cannot become two different numbers.
//
// The error policy differs between the two callers, and that asymmetry is deliberate. UsedBytes
// swallows a failed measurement and subtracts nothing, which over-counts the store and so errs
// toward evicting slightly harder - the safe direction there. A report cannot do the same: zero
// bytes reads as "nothing is accumulating", which is precisely the false reassurance this exists to
// end. AncillaryStorage therefore returns the error.

// QueueBounds is what an operator has asked one of the excluded tables to stay inside.
//
// The three bounds are independent and any of them may be zero, which means unbounded - so a table
// with no bounds at all is a table that grows until the disk does not, which is the arrangement
// item 112 exists about. Passed as a struct rather than as three parameters because the caller has
// exactly one of these per table and reads all three from configuration together.
//
// MaxBytes is the one that arrived last, and it is the only one that speaks in the unit an operator
// sizes a disk in. What makes it affordable is that these tables are already measured rather than
// scanned: for the two fixed-width ones it converts to a row cap exactly (rowsWithinBytes), and for
// the callback queue - whose rows carry a rendered payload, and memory bodies under
// callbacks.includeBodies - it is a sum over the payload_bytes column written at insert.
type QueueBounds struct {
	MaxAge   time.Duration
	MaxRows  int64
	MaxBytes int64
}

// rowsWithinBytes converts a byte cap into a row cap for a table whose rows are charged a flat
// allowance, and combines it with whatever row cap was configured beside it.
//
// This is the whole implementation of the byte cap on the forgotten log and the search outbox, and
// it is exact rather than approximate: their bytes ARE rows times an allowance, at the report, at
// the exclusion and here, so a byte cap that did anything cleverer would be bounding a figure
// nobody is shown. Zero on either side means unbounded on that side; the tighter of the two wins.
//
// The floor of one row is deliberate. A cap below a single row's allowance would otherwise resolve
// to zero, which the prune paths read as "no row cap" - so the strictest bound an operator could
// express would be no bound at all, which is the wrong direction to fail in.
func rowsWithinBytes(bounds QueueBounds, rowBytes int64) int64 {
	if bounds.MaxBytes <= 0 || rowBytes <= 0 {

		return bounds.MaxRows
	}

	rows := max(bounds.MaxBytes/rowBytes, 1)

	if bounds.MaxRows > 0 && bounds.MaxRows < rows {

		return bounds.MaxRows
	}

	return rows
}

// AncillaryTable is what one of the three excluded tables holds.
//
// Rows are reported beside Bytes because for two of the three Bytes is rows multiplied by a flat
// per-row allowance rather than a measurement (tombstoneRowBytes, outboxRowBytes), and a reader who
// can see both can see that - an estimate presented as a lone byte count invites more precision than
// it has. A count is also all that can be afforded there: scanning fixed-width rows to add up what
// their count already says would put a cost on the path that exists to bound the store, which is
// what item 25.9 is the standing reminder about.
//
// The callback queue is the exception, and it is why the pair is reported rather than a total. Its
// rows carry a rendered payload and, under callbacks.includeBodies, memory bodies, so a count says
// nothing about its size - Bytes there is the sum of the payload_bytes written at insert plus
// callbackRowOverheadBytes per row, which is a measurement of everything except the fixed part.
//
// Enabled separates the two zeroes. A feature nobody turned on holds nothing because nothing writes
// to it; an enabled one holding nothing is a queue that is keeping up. They call for opposite
// reactions, and a bare 0 says neither.
//
// It means "is recording into this table right now", NOT "this table has rows worth counting" - the
// rows are counted whenever the table exists, whatever the current setting. Disabling any of the
// three deliberately leaves what was already written in place (DeleteForgottenMemories and
// DeleteCallbackQueue are the explicit discards), so a store carrying a hundred thousand rows in a
// log it stopped writing months ago is a real state, and the one this report must not hide.
type AncillaryTable struct {
	Enabled bool
	Rows    int64
	Bytes   int64
}

// payloadBytesColumn is the column ancillaryTable sums for a table whose rows are not fixed width.
// Only the callback queue has one.
const payloadBytesColumn = "payload_bytes"

// AncillaryStorage is the three excluded tables together: the storage this store spends that its
// capacity target does not count.
type AncillaryStorage struct {
	ForgottenLog  AncillaryTable
	SearchOutbox  AncillaryTable
	CallbackQueue AncillaryTable
}

// TotalBytes is what the capacity target cannot see, all three tables together. It is the figure
// to add to consolidation.capacityBytes when sizing a disk.
func (a AncillaryStorage) TotalBytes() int64 {
	return a.ForgottenLog.Bytes + a.SearchOutbox.Bytes + a.CallbackQueue.Bytes
}

// AncillaryStorage measures the three tables UsedBytes excludes.
//
// One COUNT(*) per ENABLED table and nothing for the others, so a deployment running none of the
// three pays nothing. The caller (Server.recordAncillaryStorage) asks once per sleep cycle and
// caches the answer for GetConsolidationStatus to serve, rather than measuring per request: on the
// server dialects a count of the callback queue is a scan of up to callbacks.maxRows rows, and this
// is a figure a console polls.
//
// It is measured on every dialect, not only where UsedBytes is page accounting. The server drivers
// exclude all three by construction - usedBytesLiveRows counts memory, event and link rows
// explicitly - so the spend is exactly as invisible there, and only the reason differs.
func (d *DB) AncillaryStorage(ctx context.Context) (AncillaryStorage, error) {
	log.Trace("func() db.AncillaryStorage")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var out AncillaryStorage

	forgotten, err := d.ancillaryTable(ctx, d.tombstoneProbe())
	if err != nil {
		return AncillaryStorage{}, err
	}

	outbox, err := d.ancillaryTable(ctx, d.outboxProbe())
	if err != nil {
		return AncillaryStorage{}, err
	}

	callbacks, err := d.ancillaryTable(ctx, d.callbackProbe())
	if err != nil {
		return AncillaryStorage{}, err
	}

	out.ForgottenLog = forgotten
	out.SearchOutbox = outbox
	out.CallbackQueue = callbacks

	return out, nil
}

// ancillaryProbe is one table's measurement, as a struct rather than four parameters.
//
// exists and recording are separate for the reason AncillaryTable.Enabled gives: the count is
// gated on the TABLE, since a store that stopped recording still holds whatever it wrote, while
// what the report calls enabled is the policy. Where the two differ, the rows are real and the
// setting is not what put them there.
type ancillaryProbe struct {
	exists    bool
	recording bool
	table     string

	// rowBytes is the flat allowance charged per row. Where payloadColumn is empty it is the whole
	// row; where it is set it is the row MINUS the part being summed.
	rowBytes int64

	// payloadColumn names a column holding each row's variable-width size, summed into the total.
	// Empty for a fixed-width table, which is two of the three.
	payloadColumn string
}

func (d *DB) tombstoneProbe() ancillaryProbe {
	return ancillaryProbe{
		exists:    d.tombstoneTable,
		recording: d.tombstones.Enabled && d.tombstoneTable,
		table:     tombstonesTable,
		rowBytes:  tombstoneRowBytes,
	}
}

// The outbox has no second flag: SetSearchOutbox gates the recording and the drain together, so a
// store that is not recording into it is a store whose table is not in use at all.
func (d *DB) outboxProbe() ancillaryProbe {
	return ancillaryProbe{
		exists:    d.searchOutbox,
		recording: d.searchOutbox,
		table:     searchOutboxTable,
		rowBytes:  outboxRowBytes,
	}
}

func (d *DB) callbackProbe() ancillaryProbe {
	return ancillaryProbe{
		exists:        d.callbackTable,
		recording:     d.callbacks.Enabled && d.callbackTable,
		table:         callbackQueueTable,
		rowBytes:      callbackRowOverheadBytes,
		payloadColumn: payloadBytesColumn,
	}
}

// ancillaryTable counts one excluded table, charges it its flat per-row allowance and adds whatever
// variable-width payload it declares. A table this store does not have is not queried at all - on
// the read-only opens, and on a store that never enabled the feature, it does not exist.
//
// One statement either way: the sum rides along with the count rather than costing a second pass.
func (d *DB) ancillaryTable(ctx context.Context, probe ancillaryProbe) (AncillaryTable, error) {
	if !probe.exists {

		return AncillaryTable{}, nil
	}

	query := `SELECT COUNT(*), 0 FROM ` + probe.table

	if probe.payloadColumn != "" {
		query = `SELECT COUNT(*), COALESCE(SUM(` + probe.payloadColumn + `), 0) FROM ` + probe.table
	}

	var rows, payload int64

	if err := d.queryRow(ctx, query).Scan(&rows, &payload); err != nil {

		return AncillaryTable{}, fmt.Errorf("counting %s: %w", probe.table, err)
	}

	return AncillaryTable{
		Enabled: probe.recording,
		Rows:    rows,
		Bytes:   rows*probe.rowBytes + payload,
	}, nil
}

// excludedBytes is UsedBytes' half of the same measurement: the bytes to subtract for one table,
// counting a failed measurement as stored bytes.
//
// That is the opposite of what AncillaryStorage does with the same failure, and it is the right
// answer in each place. Here the figure is subtracted from a page count in order to keep the record
// of what was deleted from evicting live memories, so a measurement that cannot be taken leaves the
// store looking slightly larger than it is - which costs a little extra forgetting and nothing else.
func (d *DB) excludedBytes(ctx context.Context, probe ancillaryProbe) int64 {
	measured, err := d.ancillaryTable(ctx, probe)
	if err != nil {
		log.Warnf("failed to measure %s, counting it as stored bytes: %s", probe.table, err.Error())

		return 0
	}

	return measured.Bytes
}

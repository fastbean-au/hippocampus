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

// AncillaryBinding names the cap that is currently deciding what one of the excluded tables drops.
//
// It exists because the caps are a set of independent bounds with no stated precedence, and which
// one binds depends on a rate nobody can see when choosing them: a deployment forgetting nineteen
// thousand memories a day, configured maxRows 100,000 and maxAgeInDays 30, is holding five days of
// a window it believes is thirty. Both caps are enforced, neither is violated, and nothing said
// which one was in force (TODO-2 item 126).
//
// A string rather than an enum, in the mould of CycleReport.trigger: it is meant to be shown.
type AncillaryBinding string

const (
	// BindingNone means no cap can act on this table - it grows until the disk does not.
	BindingNone AncillaryBinding = "none"

	// BindingRows and BindingBytes mean the table is sitting at its effective row cap, named by
	// whichever setting produced it. A byte cap on a fixed-width table IS a row cap, so the two are
	// the same mechanism reported under the name the operator set.
	BindingRows  AncillaryBinding = "rows"
	BindingBytes AncillaryBinding = "bytes"

	// BindingAge means the table is inside its row cap, so the age cap is the only bound that can be
	// removing anything. It says nothing about whether the age cap is REACHED: a table younger than
	// its window is bound by nothing yet, and a caller wanting to know reads Oldest.
	BindingAge AncillaryBinding = "age"
)

// effectiveRowCap resolves the row cap actually in force on a table whose rows are charged a flat
// allowance, and names which of the two settings produced it.
//
// This is the whole implementation of the byte cap on the forgotten log and the search outbox, and
// it is exact rather than approximate: their bytes ARE rows times an allowance, at the report, at
// the exclusion and here, so a byte cap that did anything cleverer would be bounding a figure
// nobody is shown. Zero on either side means unbounded on that side; the tighter of the two wins.
//
// The floor of one row is deliberate. A cap below a single row's allowance would otherwise resolve
// to zero, which the prune paths read as "no row cap" - so the strictest bound an operator could
// express would be no bound at all, which is the wrong direction to fail in.
//
// It returns the name as well as the number so that the report and the enforcement cannot disagree
// about which setting is biting. Deriving that a second time from the two bounds is exactly the
// duplication that lets a report say "rows" while the prune applies the byte cap.
func effectiveRowCap(bounds QueueBounds, rowBytes int64) (int64, AncillaryBinding) {
	fromBytes := int64(0)

	if bounds.MaxBytes > 0 && rowBytes > 0 {
		fromBytes = max(bounds.MaxBytes/rowBytes, 1)
	}

	switch {

	case fromBytes > 0 && bounds.MaxRows > 0 && bounds.MaxRows <= fromBytes:
		return bounds.MaxRows, BindingRows

	case fromBytes > 0:
		return fromBytes, BindingBytes

	case bounds.MaxRows > 0:
		return bounds.MaxRows, BindingRows
	}

	return 0, BindingNone
}

// rowsWithinBytes is effectiveRowCap's number alone, for the prune paths, which want the cutoff and
// not the reason.
func rowsWithinBytes(bounds QueueBounds, rowBytes int64) int64 {
	rowCap, _ := effectiveRowCap(bounds, rowBytes)

	return rowCap
}

// bindingLimit decides which cap is holding a table where it is.
//
// The test is the row cap, and it is decisive rather than approximate: a table at or over its
// effective row cap is being held there by that cap, whatever the age cap says. Below it, an age cap
// is by elimination the only bound that can be removing anything - there is no need to compare the
// table's span against the window, and comparing them would be wrong anyway, since the prune has
// just removed everything older and the span is always a little UNDER the cap it is bound by.
func bindingLimit(bounds QueueBounds, rowBytes int64, rows int64) (AncillaryBinding, int64) {
	rowCap, source := effectiveRowCap(bounds, rowBytes)

	switch {

	case rowCap > 0 && rows >= rowCap:
		return source, rowCap

	case bounds.MaxAge > 0:
		return BindingAge, rowCap
	}

	return BindingNone, rowCap
}

// AncillaryTable is what one of the three excluded tables holds.
//
// Rows are reported beside Bytes because for two of the three Bytes is rows multiplied by a flat
// per-row allowance rather than a measurement (dialect.tombstoneRowBytes, dialect.outboxRowBytes),
// and a reader who can see both can see that - an estimate presented as a lone byte count invites
// more precision than it has. A count is also all that can be afforded there: scanning fixed-width
// rows to add up what their count already says would put a cost on the path that exists to bound the
// store, which is what item 25.9 is the standing reminder about.
//
// The callback queue is the exception, and it is why the pair is reported rather than a total. Its
// rows carry a rendered payload and, under callbacks.includeBodies, memory bodies, so a count says
// nothing about its size - Bytes there is the sum of the payload_bytes written at insert plus
// callbackRowOverheadBytes per row, which is a measurement of everything except the fixed part.
//
// Bytes and DiskBytes are two different questions and both want asking. Bytes is the table's
// STRUCTURAL size - what its rows and indexes occupy once the engine has compacted them - and it is
// the currency the byte caps are enforced in, which is why it has to be an allowance rather than a
// reading (dialect.tombstoneRowBytes). DiskBytes is what the engine says the relation really
// occupies right now, unreclaimed space included, and is 0 where the dialect cannot answer cheaply.
// On a server dialect under steady churn the second is routinely twice the first, and the gap is
// itself the finding: a live Postgres store held 100,002 tombstones structurally worth 25 MB in 46 MB
// of relation. On the embedded dialect they are the same number, and only one is reported, because a
// page freed by a prune returns to the freelist that UsedBytes already excludes.
//
// Oldest is the UnixNano of the oldest row, or 0 when the table is empty. It is what turns a row
// count into a retention window: a log holding its row cap says nothing about how much history that
// is, and the whole of TODO-2 item 126 is a deployment that asked for thirty days, was given five,
// and had no way to find out.
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
	Enabled   bool
	Rows      int64
	Bytes     int64
	DiskBytes int64
	Oldest    int64

	// The caps in force, reported beside the figures they bound rather than read separately at
	// render time, so a measurement and its bounds are always the pair that were true together.
	// Zero on any of them means that bound is not set.
	//
	// LimitRows is the EFFECTIVE row cap - the tighter of the configured row cap and whatever the
	// byte cap resolves to - and Binding names which of the three is actually deciding what this
	// table drops. See AncillaryBinding.
	LimitRows  int64
	LimitBytes int64
	LimitAge   time.Duration
	Binding    AncillaryBinding
}

// Footprint is what this table costs the disk: the engine's own reading where there is one, and the
// structural estimate where there is not. It is what the gauge publishes and what a total is summed
// from, because "how much disk does this deployment need" has one answer and it is the engine's.
//
// The measurement usually EXCEEDS the estimate, by whatever the engine has not reclaimed, and that is
// the case this pair was added for. It can also fall below it - the callback queue's Bytes are summed
// from payload lengths written at insert, and a large payload is stored compressed - and the
// measurement is still the right answer there, being the one taken from the disk.
func (t AncillaryTable) Footprint() int64 {
	if t.DiskBytes > 0 {

		return t.DiskBytes
	}

	return t.Bytes
}

// payloadBytesColumn is the column ancillaryTable sums for a table whose rows are not fixed width.
// Only the callback queue has one.
const payloadBytesColumn = "payload_bytes"

// AncillaryBounds are the caps the SERVICE holds for two of the three excluded tables, passed in so
// the report states the bounds that are actually enforced rather than a second reading of them.
//
// The forgotten log is deliberately absent: its policy is installed on the store
// (SetTombstonePolicy) and is what PruneTombstones applies, so taking it from anywhere else is how a
// report comes to disagree with the prune it describes. The other two are passed to their prune
// calls per cycle rather than installed, and this mirrors that.
type AncillaryBounds struct {
	SearchOutbox  QueueBounds
	CallbackQueue QueueBounds
}

// AncillaryStorage is the three excluded tables together: the storage this store spends that its
// capacity target does not count.
type AncillaryStorage struct {
	ForgottenLog  AncillaryTable
	SearchOutbox  AncillaryTable
	CallbackQueue AncillaryTable
}

// TotalBytes is what the capacity target cannot see, all three tables together. It is the figure
// to add to consolidation.capacityBytes when sizing a disk, so it sums Footprint rather than Bytes:
// a disk is sized against what the engine is holding, not against what the rows would occupy if it
// were compacted. It is therefore not always the sum of the three Bytes figures.
func (a AncillaryStorage) TotalBytes() int64 {
	return a.ForgottenLog.Footprint() + a.SearchOutbox.Footprint() + a.CallbackQueue.Footprint()
}

// AncillaryStorage measures the three tables UsedBytes excludes.
//
// One aggregate per table that EXISTS and nothing for the others, plus a catalogue lookup where the
// dialect can measure a relation - so a deployment running none of the three pays nothing, and one
// running all of them pays six statements a cycle. The caller (Server.recordAncillaryStorage) asks
// once per sleep cycle and caches the answer for GetConsolidationStatus to serve, rather than
// measuring per request: on the server dialects a count of the callback queue is a scan of up to
// callbacks.maxRows rows, and this is a figure a console polls.
//
// It is measured on every dialect, not only where UsedBytes is page accounting. The server drivers
// exclude all three by construction - usedBytesLiveRows counts memory, event and link rows
// explicitly - so the spend is exactly as invisible there, and only the reason differs.
func (d *DB) AncillaryStorage(ctx context.Context, bounds AncillaryBounds) (AncillaryStorage, error) {
	log.Trace("func() db.AncillaryStorage")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var out AncillaryStorage

	forgotten, err := d.ancillaryTable(ctx, d.tombstoneProbe(), d.tombstones.bounds())
	if err != nil {
		return AncillaryStorage{}, err
	}

	outbox, err := d.ancillaryTable(ctx, d.outboxProbe(), bounds.SearchOutbox)
	if err != nil {
		return AncillaryStorage{}, err
	}

	callbacks, err := d.ancillaryTable(ctx, d.callbackProbe(), bounds.CallbackQueue)
	if err != nil {
		return AncillaryStorage{}, err
	}

	out.ForgottenLog = forgotten
	out.SearchOutbox = outbox
	out.CallbackQueue = callbacks

	return out, nil
}

// ancillaryProbe is what is needed to measure one table, as a struct rather than a list of
// parameters.
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

	// ageColumn names the UnixNano column the table's age cap is applied to, and so the column whose
	// minimum says how far back the table actually reaches. All three have one; it is a field rather
	// than a constant because they do not agree on its name.
	ageColumn string
}

func (d *DB) tombstoneProbe() ancillaryProbe {
	return ancillaryProbe{
		exists:    d.tombstoneTable,
		recording: d.tombstones.Enabled && d.tombstoneTable,
		table:     tombstonesTable,
		rowBytes:  d.dialect().tombstoneRowBytes,
		ageColumn: "forgotten_at",
	}
}

// The outbox has no second flag: SetSearchOutbox gates the recording and the drain together, so a
// store that is not recording into it is a store whose table is not in use at all.
func (d *DB) outboxProbe() ancillaryProbe {
	return ancillaryProbe{
		exists:    d.searchOutbox,
		recording: d.searchOutbox,
		table:     searchOutboxTable,
		rowBytes:  d.dialect().outboxRowBytes,
		ageColumn: "queued_at",
	}
}

func (d *DB) callbackProbe() ancillaryProbe {
	return ancillaryProbe{
		exists:        d.callbackTable,
		recording:     d.callbacks.Enabled && d.callbackTable,
		table:         callbackQueueTable,
		rowBytes:      callbackRowOverheadBytes,
		payloadColumn: payloadBytesColumn,
		ageColumn:     "queued_at",
	}
}

// ancillaryTable counts one excluded table, charges it its flat per-row allowance, adds whatever
// variable-width payload it declares, and reads how far back it reaches. A table this store does not
// have is not queried at all - on the read-only opens, and on a store that never enabled the
// feature, it does not exist.
//
// One statement for all of it: the payload sum and the oldest row ride along with the count rather
// than costing a pass each. The relation's real size is a second statement where the dialect offers
// one, and a catalogue lookup rather than a scan - see dialect.relationBytes.
func (d *DB) ancillaryTable(
	ctx context.Context,
	probe ancillaryProbe,
	bounds QueueBounds,
) (AncillaryTable, error) {
	if !probe.exists {

		return AncillaryTable{}, nil
	}

	payloadTerm := "0"
	if probe.payloadColumn != "" {
		payloadTerm = `COALESCE(SUM(` + probe.payloadColumn + `), 0)`
	}

	// MIN over an empty table is NULL on every dialect, which COALESCE turns into the 0 an empty
	// table is meant to report.
	oldestTerm := "0"
	if probe.ageColumn != "" {
		oldestTerm = `COALESCE(MIN(` + probe.ageColumn + `), 0)`
	}

	var rows, payload, oldest int64

	if err := d.queryRow(
		ctx,
		`SELECT COUNT(*), `+payloadTerm+`, `+oldestTerm+` FROM `+probe.table,
	).Scan(&rows, &payload, &oldest); err != nil {

		return AncillaryTable{}, fmt.Errorf("counting %s: %w", probe.table, err)
	}

	disk, err := d.relationBytes(ctx, probe.table)
	if err != nil {

		return AncillaryTable{}, err
	}

	binding, rowCap := bindingLimit(bounds, probe.rowBytes, rows)

	return AncillaryTable{
		Enabled:    probe.recording,
		Rows:       rows,
		Bytes:      rows*probe.rowBytes + payload,
		DiskBytes:  disk,
		Oldest:     oldest,
		LimitRows:  rowCap,
		LimitBytes: bounds.MaxBytes,
		LimitAge:   bounds.MaxAge,
		Binding:    binding,
	}, nil
}

// relationBytes asks the engine what one table and everything built over it really occupy. It
// answers 0 where the dialect has no cheap way to say, which is the embedded one - and where the
// answer would be the structural estimate anyway, since a page a prune frees goes to the freelist
// UsedBytes already excludes.
//
// The reading includes space the engine has not returned to the filesystem, which is the whole point
// of reporting it: that space is real disk, it is invisible to every other figure the service
// publishes, and on a table under the constant insert-and-prune churn these three are it is a large
// share of what they cost. It is reported and never regulated on - a control input reading this
// would prune harder, leave more dead rows, and prune harder again. See dialect.relationBytes.
func (d *DB) relationBytes(ctx context.Context, table string) (int64, error) {
	query := d.dialect().relationBytes

	if query == "" {

		return 0, nil
	}

	var bytes int64

	if err := d.queryRow(ctx, d.rebind(query), table).Scan(&bytes); err != nil {

		return 0, fmt.Errorf("measuring %s: %w", table, err)
	}

	return bytes, nil
}

// excludedBytes is UsedBytes' half of the same measurement: the bytes to subtract for one table,
// counting a failed measurement as stored bytes.
//
// It subtracts the STRUCTURAL figure, never the footprint. Only the embedded dialect ever calls it -
// the server drivers count live memory, event and link rows explicitly and so exclude all three
// already - and on that dialect the two are the same number, so the choice costs nothing today and
// states which one a fourth dialect would want: this figure is taken off a page count in order to
// regulate eviction, and everything dialect.tombstoneRowBytes says about regulating on a reading
// that does not drop applies here.
//
// That is the opposite of what AncillaryStorage does with the same failure, and it is the right
// answer in each place. Here the figure is subtracted from a page count in order to keep the record
// of what was deleted from evicting live memories, so a measurement that cannot be taken leaves the
// store looking slightly larger than it is - which costs a little extra forgetting and nothing else.
func (d *DB) excludedBytes(ctx context.Context, probe ancillaryProbe) int64 {
	// The bounds are not read on this path - only Bytes is - so the zero value is passed rather than
	// threading a set of caps through UsedBytes to be discarded.
	measured, err := d.ancillaryTable(ctx, probe, QueueBounds{})
	if err != nil {
		log.Warnf("failed to measure %s, counting it as stored bytes: %s", probe.table, err.Error())

		return 0
	}

	return measured.Bytes
}

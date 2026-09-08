package hippocampus

import (
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// The listing predicate, built once and used by both the listing and the deletion.
//
// DeleteMemoriesByFilter and DeleteEventsByFilter promise that GetMemories/GetEvents with the same
// fields is the dry run for the deletion. A promise like that is worth nothing if the two build
// their predicates separately - the request messages carry the same field names, so a divergence
// would be invisible on the wire and would show up only as a call deleting something the listing
// had not shown.
//
// So they do not build them separately. memorySelector and eventSelector name the selecting fields
// the two request messages have in common; both proto messages satisfy their interface without any
// adapter, because the fields are named identically on purpose. The listing adds ordering,
// pagination and its link traversal on top; the deletion adds a batch bound. Everything that
// decides WHICH records are in play is here, in one function per kind.
//
// What is deliberately absent from both interfaces is order_by/order_dir/limit/offset (there is no
// page to order or skip when deleting), the response-shaping flags, and linked_to - see the
// DeleteMemoriesByFilterRequest comment in the contract for why that last one cannot be offered.

// memorySelector is the set of GetMemoriesRequest fields that decide which memories match.
// *contract.GetMemoriesRequest and *contract.DeleteMemoriesByFilterRequest both satisfy it.
type memorySelector interface {
	GetTimestampMin() int64
	GetTimestampMax() int64
	GetSignificanceMin() int32
	GetSignificanceMax() int32
	GetSignificanceExtremum() contract.SignificanceExtremum
	GetGroup() string
	GetMetadata() []string
	GetRecalled() contract.Bool
	GetRecallCountMin() int32
	GetRecallCountMax() int32
	GetTimeRecalledMin() int64
	GetTimeRecalledMax() int64
	GetIsSummary() contract.Bool
	GetIsBinary() contract.Bool
	GetEventId() string
	GetHasEvent() contract.Bool
}

// eventSelector is the set of GetEventsRequest fields that decide which events match.
// *contract.GetEventsRequest and *contract.DeleteEventsByFilterRequest both satisfy it.
type eventSelector interface {
	GetTimeStartMin() int64
	GetTimeStartMax() int64
	GetTimeEndMin() int64
	GetTimeEndMax() int64
	GetSignificanceMin() int32
	GetSignificanceMax() int32
	GetSignificanceExtremum() contract.SignificanceExtremum
	GetGroup() string
	GetMetadata() []string
	GetEnded() contract.Bool
	GetNameContains() string
}

// memorySelectionFilter validates a memory selection and turns it into a db.MemoryFilter carrying
// only the selecting dimensions: OrderBy, OrderDirection, Limit, Offset, Ids and Groups are the
// caller's to set afterwards.
//
// The range checks return bare errors rather than InvalidArgument statuses. That is not a
// preference - it is what GetMemories has always returned for them, and this function is shared
// with it, so tightening the code here would change a listing's failure mode without anybody
// asking for it. The two checks that were added later (metadata, extremum) keep their statuses.
func memorySelectionFilter(in memorySelector) (db.MemoryFilter, error) {
	var filter db.MemoryFilter

	if in.GetSignificanceMax() > 0 && in.GetSignificanceMin() > 0 && in.GetSignificanceMax() < in.GetSignificanceMin() {
		return filter, fmt.Errorf("SignificanceMax must be greater than or equal to SignificanceMin")
	}

	if in.GetTimestampMax() > 0 && in.GetTimestampMin() > 0 && in.GetTimestampMax() < in.GetTimestampMin() {
		return filter, fmt.Errorf("TimestampMax must be greater than or equal to TimestampMin")
	}

	if in.GetRecallCountMax() > 0 && in.GetRecallCountMin() > 0 && in.GetRecallCountMax() < in.GetRecallCountMin() {
		return filter, fmt.Errorf("RecallCountMax must be greater than or equal to RecallCountMin")
	}

	if in.GetTimeRecalledMax() > 0 && in.GetTimeRecalledMin() > 0 && in.GetTimeRecalledMax() < in.GetTimeRecalledMin() {
		return filter, fmt.Errorf("TimeRecalledMax must be greater than or equal to TimeRecalledMin")
	}

	// Filter keys are validated here, not only at the db layer, because an unvalidated key reaches
	// the driver as part of a JSON path: MySQL's JSON_EXTRACT raises ER_INVALID_JSON_PATH on a
	// malformed one, which would surface as Internal rather than as the caller's mistake.
	metadata, err := types.ParseMetadataFilters(in.GetMetadata())
	if err != nil {
		return filter, status.Error(codes.InvalidArgument, err.Error())
	}

	extremum := significanceExtremum(in.GetSignificanceExtremum())

	if extremum != db.SignificanceExtremumNone && (in.GetSignificanceMin() > 0 || in.GetSignificanceMax() > 0) {
		return filter, fmt.Errorf("SignificanceExtremum cannot be combined with SignificanceMin/SignificanceMax")
	}

	filter = db.MemoryFilter{
		TimeStampMin:         in.GetTimestampMin(),
		TimeStampMax:         in.GetTimestampMax(),
		SignificanceMin:      in.GetSignificanceMin(),
		SignificanceMax:      in.GetSignificanceMax(),
		SignificanceExtremum: extremum,
		Group:                in.GetGroup(),

		EventId: in.GetEventId(),

		Metadata:        metadata,
		Recalled:        triState(in.GetRecalled()),
		HasEvent:        triState(in.GetHasEvent()),
		IsSummary:       triState(in.GetIsSummary()),
		IsBinary:        triState(in.GetIsBinary()),
		RecallCountMin:  in.GetRecallCountMin(),
		RecallCountMax:  in.GetRecallCountMax(),
		TimeRecalledMin: in.GetTimeRecalledMin(),
		TimeRecalledMax: in.GetTimeRecalledMax(),
	}

	return filter, nil
}

// eventSelectionFilter is memorySelectionFilter's counterpart for events, on the same terms.
func eventSelectionFilter(in eventSelector) (db.EventFilter, error) {
	var filter db.EventFilter

	if in.GetSignificanceMax() > 0 && in.GetSignificanceMin() > 0 && in.GetSignificanceMax() < in.GetSignificanceMin() {
		return filter, fmt.Errorf("SignificanceMax must be greater than or equal to SignificanceMin")
	}

	if in.GetTimeStartMax() > 0 && in.GetTimeStartMin() > 0 && in.GetTimeStartMax() < in.GetTimeStartMin() {
		return filter, fmt.Errorf("TimeStartMax must be greater than or equal to TimeStartMin")
	}

	if in.GetTimeEndMax() > 0 && in.GetTimeEndMin() > 0 && in.GetTimeEndMax() < in.GetTimeEndMin() {
		return filter, fmt.Errorf("TimeEndMax must be greater than or equal to TimeEndMin")
	}

	if in.GetTimeStartMin() > 0 && in.GetTimeEndMin() > 0 && in.GetTimeEndMin() < in.GetTimeStartMin() {
		return filter, fmt.Errorf("TimeEndMin must be greater than or equal to TimeStartMin")
	}

	if in.GetTimeStartMax() > 0 && in.GetTimeEndMax() > 0 && in.GetTimeEndMax() < in.GetTimeStartMax() {
		return filter, fmt.Errorf("TimeEndMax must be greater than or equal to TimeStartMax")
	}

	// See memorySelectionFilter for why filter keys are validated here rather than only at the db
	// layer.
	metadata, err := types.ParseMetadataFilters(in.GetMetadata())
	if err != nil {
		return filter, status.Error(codes.InvalidArgument, err.Error())
	}

	extremum := significanceExtremum(in.GetSignificanceExtremum())

	if extremum != db.SignificanceExtremumNone && (in.GetSignificanceMin() > 0 || in.GetSignificanceMax() > 0) {
		return filter, fmt.Errorf("SignificanceExtremum cannot be combined with SignificanceMin/SignificanceMax")
	}

	filter = db.EventFilter{
		TimeStartMin:         in.GetTimeStartMin(),
		TimeStartMax:         in.GetTimeStartMax(),
		TimeEndMin:           in.GetTimeEndMin(),
		TimeEndMax:           in.GetTimeEndMax(),
		SignificanceMin:      in.GetSignificanceMin(),
		SignificanceMax:      in.GetSignificanceMax(),
		SignificanceExtremum: extremum,
		Group:                in.GetGroup(),

		Metadata:     metadata,
		Ended:        triState(in.GetEnded()),
		NameContains: in.GetNameContains(),
	}

	return filter, nil
}

// significanceExtremum maps the wire enum onto the storage layer's, shared by both builders.
func significanceExtremum(in contract.SignificanceExtremum) db.SignificanceExtremum {
	switch in {

	case contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST:
		return db.SignificanceExtremumHighest

	case contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_LOWEST:
		return db.SignificanceExtremumLowest

	}

	return db.SignificanceExtremumNone
}

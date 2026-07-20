package contract_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
)

// messageTypes lists every proto message struct defined in hippocampus.proto whose Get* accessors
// are exercisable generically via reflection: construct a populated instance by setting every
// exported field to a distinct, kind-appropriate value, then assert each Get<Field> method returns
// exactly that value; separately assert every Get<Field> method is nil-safe (returns the zero value
// without panicking) when called through a nil pointer of the same type.
//
// ArchiveRecord (and its three oneof wrapper types) is deliberately excluded: its Record field is a
// oneof interface, not a plain settable field, so it gets its own dedicated test below.
// EmptyRequest is excluded too: it has no fields and therefore no Get* methods to exercise.
var messageTypes = []reflect.Type{
	reflect.TypeOf(contract.SignificancePlacement{}),
	reflect.TypeOf(contract.Event{}),
	reflect.TypeOf(contract.Relationship{}),
	reflect.TypeOf(contract.Memory{}),
	reflect.TypeOf(contract.StoreEventResponse{}),
	reflect.TypeOf(contract.EndEventRequest{}),
	reflect.TypeOf(contract.UpdateEventSignificanceRequest{}),
	reflect.TypeOf(contract.MergeEventsRequest{}),
	reflect.TypeOf(contract.DeleteEventRequest{}),
	reflect.TypeOf(contract.GetEventByIdRequest{}),
	reflect.TypeOf(contract.GetEventResponse{}),
	reflect.TypeOf(contract.GetEventsRequest{}),
	reflect.TypeOf(contract.GetEventsResponse{}),
	reflect.TypeOf(contract.GetMemoriesRequest{}),
	reflect.TypeOf(contract.GetMemoriesResponse{}),
	reflect.TypeOf(contract.StoreMemoryResponse{}),
	reflect.TypeOf(contract.DeleteMemoriesRequest{}),
	reflect.TypeOf(contract.RecallMemoriesRequest{}),
	reflect.TypeOf(contract.SearchMemoriesRequest{}),
	reflect.TypeOf(contract.ReplaceMemoriesWithSummaryRequest{}),
	reflect.TypeOf(contract.ReplaceMemoriesWithSummaryResponse{}),
	reflect.TypeOf(contract.SummarizationCandidate{}),
	reflect.TypeOf(contract.GetSummarizationCandidatesResponse{}),
	reflect.TypeOf(contract.ArchiveHeader{}),
	reflect.TypeOf(contract.ImportBatchRequest{}),
	reflect.TypeOf(contract.ImportBatchResponse{}),
	reflect.TypeOf(contract.ExportRequest{}),
	reflect.TypeOf(contract.ExportResponse{}),
	reflect.TypeOf(contract.ImportRequest{}),
	reflect.TypeOf(contract.ImportResponse{}),
	reflect.TypeOf(contract.TransferRequest{}),
	reflect.TypeOf(contract.TransferResponse{}),
	reflect.TypeOf(contract.ClearRequest{}),
	reflect.TypeOf(contract.ClearResponse{}),
	reflect.TypeOf(contract.GeneralResponse{}),
}

// TestMessageGetters drives every message type above through the nil-safety and populated-value
// checks. It is table-driven over messageTypes rather than hand-written per message: the generated
// Get<Field> methods are entirely mechanical (nil-check, return the field, else zero value), so a
// single reflection-based driver covers all of them without hundreds of near-identical assertions.
func TestMessageGetters(t *testing.T) {
	for _, typ := range messageTypes {
		typ := typ

		t.Run(typ.Name(), func(t *testing.T) {
			t.Run("nil", func(t *testing.T) { assertNilGettersAreSafe(t, typ) })
			t.Run("populated", func(t *testing.T) { assertPopulatedGettersMatchFields(t, typ) })
		})
	}
}

// assertNilGettersAreSafe calls every Get<Field> method on a typed nil pointer and asserts it
// neither panics nor returns anything but the zero value of its return type - the nil-safe
// behaviour documented on every generated getter (e.g. `func (x *Memory) GetId() string { if x !=
// nil { return x.Id }; return "" }`).
func assertNilGettersAreSafe(t *testing.T, typ reflect.Type) {
	t.Helper()

	ptrType := reflect.PointerTo(typ)
	nilPtr := reflect.Zero(ptrType)

	for i := 0; i < ptrType.NumMethod(); i++ {
		method := ptrType.Method(i)

		if !strings.HasPrefix(method.Name, "Get") {
			continue
		}

		if method.Func.Type().NumIn() != 1 || method.Func.Type().NumOut() != 1 {
			continue
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s.%s panicked on a nil receiver: %v", typ.Name(), method.Name, r)
				}
			}()

			out := method.Func.Call([]reflect.Value{nilPtr})[0]
			zero := reflect.Zero(out.Type())

			if !reflect.DeepEqual(out.Interface(), zero.Interface()) {
				t.Errorf("%s.%s() on nil = %#v, want zero value %#v", typ.Name(), method.Name, out.Interface(), zero.Interface())
			}
		}()
	}
}

// assertPopulatedGettersMatchFields builds a populated *typ instance (every exported field set to a
// distinct, kind-appropriate value) and asserts every Get<Field> method returns exactly the value
// stored in the matching field - the generated getters' one substantive branch.
func assertPopulatedGettersMatchFields(t *testing.T, typ reflect.Type) {
	t.Helper()

	instance := reflect.New(typ)
	elem := instance.Elem()

	fieldValues := make(map[string]reflect.Value)

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)

		if field.PkgPath != "" {
			// unexported (state/unknownFields/sizeCache bookkeeping) - not a proto field.
			continue
		}

		value := valueForType(t, field.Type, i)
		elem.Field(i).Set(value)
		fieldValues[field.Name] = value
	}

	if len(fieldValues) == 0 {
		t.Fatalf("%s has no exported fields to exercise - should it be in messageTypes at all?", typ.Name())
	}

	for name, want := range fieldValues {
		method := instance.MethodByName("Get" + name)
		if !method.IsValid() {
			t.Errorf("no Get%s() method found for field %s.%s", name, typ.Name(), name)

			continue
		}

		out := method.Call(nil)[0]

		if !reflect.DeepEqual(out.Interface(), want.Interface()) {
			t.Errorf("%s.Get%s() = %#v, want %#v", typ.Name(), name, out.Interface(), want.Interface())
		}
	}
}

// valueForType builds a distinct, deterministic value of the given field type, seeded by index so
// sibling fields of the same kind (e.g. two int32 fields) get different values - a getter that
// accidentally read the wrong field would otherwise go unnoticed.
func valueForType(t *testing.T, fieldType reflect.Type, seed int) reflect.Value {
	t.Helper()

	switch fieldType.Kind() {

	case reflect.String:
		return reflect.ValueOf(fmt.Sprintf("value-%d", seed)).Convert(fieldType)

	case reflect.Bool:
		return reflect.ValueOf(true)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v := reflect.New(fieldType).Elem()
		v.SetInt(int64(seed + 1))

		return v

	case reflect.Slice:
		elemType := fieldType.Elem()
		slice := reflect.MakeSlice(fieldType, 1, 1)

		switch elemType.Kind() {

		case reflect.String:
			slice.Index(0).Set(valueForType(t, elemType, seed))

		case reflect.Pointer:
			slice.Index(0).Set(reflect.New(elemType.Elem()))

		default:
			t.Fatalf("valueForType: unsupported slice element kind %s for %s", elemType.Kind(), fieldType)
		}

		return slice

	case reflect.Pointer:
		return reflect.New(fieldType.Elem())

	default:
		t.Fatalf("valueForType: unsupported field kind %s for %s", fieldType.Kind(), fieldType)

		return reflect.Value{}
	}
}

// TestArchiveRecordGetters covers ArchiveRecord separately: its Record field is a oneof interface
// (isArchiveRecord_Record), populated by one of three generated wrapper types
// (ArchiveRecord_Header/_Event/_Memory) rather than by a plain settable field, so it falls outside
// the generic reflection driver above. This exercises GetRecord/GetHeader/GetEvent/GetMemory on a
// nil receiver, and on each of the three populated oneof cases - including the "wrong case" type
// assertion failing and falling through to the zero value, which the generic driver never reaches.
func TestArchiveRecordGetters(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		var rec *contract.ArchiveRecord

		if got := rec.GetRecord(); got != nil {
			t.Errorf("GetRecord() on nil = %#v, want nil", got)
		}

		if got := rec.GetHeader(); got != nil {
			t.Errorf("GetHeader() on nil = %#v, want nil", got)
		}

		if got := rec.GetEvent(); got != nil {
			t.Errorf("GetEvent() on nil = %#v, want nil", got)
		}

		if got := rec.GetMemory(); got != nil {
			t.Errorf("GetMemory() on nil = %#v, want nil", got)
		}
	})

	t.Run("header", func(t *testing.T) {
		header := &contract.ArchiveHeader{Version: 1}
		rec := &contract.ArchiveRecord{Record: &contract.ArchiveRecord_Header{Header: header}}

		if got := rec.GetHeader(); got != header {
			t.Errorf("GetHeader() = %#v, want %#v", got, header)
		}

		if got := rec.GetEvent(); got != nil {
			t.Errorf("GetEvent() on a header record = %#v, want nil", got)
		}

		if got := rec.GetMemory(); got != nil {
			t.Errorf("GetMemory() on a header record = %#v, want nil", got)
		}

		if got := rec.GetRecord(); got != rec.Record {
			t.Errorf("GetRecord() = %#v, want %#v", got, rec.Record)
		}
	})

	t.Run("event", func(t *testing.T) {
		event := &contract.Event{Id: "e1"}
		rec := &contract.ArchiveRecord{Record: &contract.ArchiveRecord_Event{Event: event}}

		if got := rec.GetEvent(); got != event {
			t.Errorf("GetEvent() = %#v, want %#v", got, event)
		}

		if got := rec.GetHeader(); got != nil {
			t.Errorf("GetHeader() on an event record = %#v, want nil", got)
		}

		if got := rec.GetMemory(); got != nil {
			t.Errorf("GetMemory() on an event record = %#v, want nil", got)
		}
	})

	t.Run("memory", func(t *testing.T) {
		memory := &contract.Memory{Id: "m1"}
		rec := &contract.ArchiveRecord{Record: &contract.ArchiveRecord_Memory{Memory: memory}}

		if got := rec.GetMemory(); got != memory {
			t.Errorf("GetMemory() = %#v, want %#v", got, memory)
		}

		if got := rec.GetHeader(); got != nil {
			t.Errorf("GetHeader() on a memory record = %#v, want nil", got)
		}

		if got := rec.GetEvent(); got != nil {
			t.Errorf("GetEvent() on a memory record = %#v, want nil", got)
		}
	})
}

// TestArchiveRecordMarshalRoundTrip proto-marshals then unmarshals an ArchiveRecord for each of its
// three oneof variants (and a standalone ArchiveHeader), asserting the round trip is lossless. This
// is the wire encoding archive.Export/archive.Import actually depend on (a gzip+protodelim stream of
// these exact records), so unlike a plain getter check it is exercising real, load-bearing behaviour
// - and it is what finally exercises the oneof wrapper types' wire-format plumbing
// (ArchiveRecord_Header/_Event/_Memory), never reached by any of the gateway/gRPC walks since
// ArchiveRecord never appears in a request or response message.
func TestArchiveRecordMarshalRoundTrip(t *testing.T) {
	t.Run("header", func(t *testing.T) {
		want := &contract.ArchiveRecord{Record: &contract.ArchiveRecord_Header{
			Header: &contract.ArchiveHeader{Version: 1},
		}}

		data, err := proto.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal: %s", err)
		}

		got := &contract.ArchiveRecord{}
		if err := proto.Unmarshal(data, got); err != nil {
			t.Fatalf("Unmarshal: %s", err)
		}

		if got.GetHeader().GetVersion() != want.GetHeader().GetVersion() {
			t.Errorf("GetHeader().GetVersion() = %d, want %d", got.GetHeader().GetVersion(), want.GetHeader().GetVersion())
		}
	})

	t.Run("event", func(t *testing.T) {
		want := &contract.ArchiveRecord{Record: &contract.ArchiveRecord_Event{
			Event: &contract.Event{Id: "e1", Name: "roundtrip event", Significance: 5},
		}}

		data, err := proto.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal: %s", err)
		}

		got := &contract.ArchiveRecord{}
		if err := proto.Unmarshal(data, got); err != nil {
			t.Fatalf("Unmarshal: %s", err)
		}

		if got.GetEvent().GetId() != want.GetEvent().GetId() || got.GetEvent().GetName() != want.GetEvent().GetName() {
			t.Errorf("GetEvent() = %#v, want %#v", got.GetEvent(), want.GetEvent())
		}
	})

	t.Run("memory", func(t *testing.T) {
		want := &contract.ArchiveRecord{Record: &contract.ArchiveRecord_Memory{
			Memory: &contract.Memory{Id: "m1", Body: "roundtrip memory", Significance: 3},
		}}

		data, err := proto.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal: %s", err)
		}

		got := &contract.ArchiveRecord{}
		if err := proto.Unmarshal(data, got); err != nil {
			t.Fatalf("Unmarshal: %s", err)
		}

		if got.GetMemory().GetId() != want.GetMemory().GetId() || got.GetMemory().GetBody() != want.GetMemory().GetBody() {
			t.Errorf("GetMemory() = %#v, want %#v", got.GetMemory(), want.GetMemory())
		}
	})

	t.Run("header standalone", func(t *testing.T) {
		want := &contract.ArchiveHeader{Version: 2}

		data, err := proto.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal: %s", err)
		}

		got := &contract.ArchiveHeader{}
		if err := proto.Unmarshal(data, got); err != nil {
			t.Fatalf("Unmarshal: %s", err)
		}

		if got.GetVersion() != want.GetVersion() {
			t.Errorf("GetVersion() = %d, want %d", got.GetVersion(), want.GetVersion())
		}
	})
}

// TestSwaggerJSON is a trivial sanity check on the embedded OpenAPI document: it's baked in as a
// single []byte constant (swagger.go), so there's no real logic to exercise beyond "it's there and
// it's valid JSON".
func TestSwaggerJSON(t *testing.T) {
	if len(contract.SwaggerJSON) == 0 {
		t.Fatal("contract.SwaggerJSON is empty")
	}

	var doc map[string]any
	if err := json.Unmarshal(contract.SwaggerJSON, &doc); err != nil {
		t.Fatalf("contract.SwaggerJSON is not valid JSON: %s", err)
	}
}

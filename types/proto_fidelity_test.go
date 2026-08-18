package types

import (
	"reflect"
	"testing"
)

// oneWayMemoryFields are the fields a Memory -> proto -> Memory trip is NOT expected to preserve,
// each for a stated reason. Anything not listed here must survive.
var oneWayMemoryFields = map[string]string{
	// Store-maintained: ToProto carries it out so a client can read it, FromProto deliberately
	// refuses to read it back in, since a client may not set it.
	"LinkSignificance": "calculated by the store, never accepted from a client",

	// Write-only update instructions. They exist because every other field reads its zero value as
	// "leave unchanged", so they are never populated on a read and have nowhere to come back from.
	"ClearMetadata": "write-only update instruction",
	"ClearGroup":    "write-only update instruction",

	// Internal: resolved by the RPC layer against the significance registry and never on the wire.
	// (Relative placement arrives on the REQUEST message, not on Memory, so there is nothing here
	// for it to name.)
	"SignificanceLevelID": "internal, resolved server-side and not part of the contract",
}

var oneWayEventFields = map[string]string{
	// Documented in the contract as "read-only ... ignored on write", so FromProto is right to drop
	// it. The archive is the one caller that needs it back, and restores it at its own site -
	// ingestEvents sets it from the proto explicitly, because a full-state import is not a write.
	"MemoriesConsolidated": "read-only in the contract; the import restores it explicitly",

	"LinkSignificance":    "calculated by the store, never accepted from a client",
	"ClearMetadata":       "write-only update instruction",
	"ClearGroup":          "write-only update instruction",
	"SignificanceLevelID": "internal, resolved server-side and not part of the contract",
}

// TestProtoConversionCarriesEveryField is a drift guard over the conversions the archive, the RPC
// layer and every client depend on.
//
// The hand-written round-trip tests beside this one assert the fields they happen to name, which is
// the failure mode worth guarding: a field added to the struct and to the proto but forgotten in
// ToProto or FromProto round-trips as zero-equals-zero and passes them all. It is not a crash and
// not a test failure - it is an export that quietly drops a column, discovered when someone restores
// from it.
//
// So this populates EVERY field non-zero by reflection and requires each one back, with the
// deliberate exceptions listed above naming their reason. A new field is covered the moment it
// exists; a new exception has to be argued for in writing.
func TestProtoConversionCarriesEveryField(t *testing.T) {
	// A stale exception - one naming a field that no longer exists - would be skipped in silence and
	// would go on excusing nothing, so the lists are checked against the structs first.
	assertFieldsExist(t, reflect.TypeOf(Memory{}), oneWayMemoryFields)
	assertFieldsExist(t, reflect.TypeOf(Event{}), oneWayEventFields)

	t.Run("memory", func(t *testing.T) {
		var memory Memory

		populate(t, reflect.ValueOf(&memory).Elem())

		back := MemoryFromProto(memory.ToProto())

		compareFields(t, reflect.ValueOf(memory), reflect.ValueOf(back), oneWayMemoryFields)
	})

	t.Run("event", func(t *testing.T) {
		var event Event

		populate(t, reflect.ValueOf(&event).Elem())

		back := EventFromProto(event.ToProto())

		compareFields(t, reflect.ValueOf(event), reflect.ValueOf(back), oneWayEventFields)
	})
}

// assertFieldsExist requires every name in a one-way list to be a real field of the struct.
func assertFieldsExist(t *testing.T, structType reflect.Type, oneWay map[string]string) {
	t.Helper()

	for name := range oneWay {
		if _, ok := structType.FieldByName(name); !ok {
			t.Errorf("%s lists a one-way field %q that does not exist - remove it", structType.Name(), name)
		}
	}
}

// compareFields reports every exported field that did not survive the trip, and every field listed
// as one-way that in fact did - so the exception list cannot outlive its reason either.
func compareFields(t *testing.T, before reflect.Value, after reflect.Value, oneWay map[string]string) {
	t.Helper()

	structType := before.Type()

	for i := range before.NumField() {
		field := structType.Field(i)

		if !field.IsExported() {
			continue
		}

		got, want := after.Field(i).Interface(), before.Field(i).Interface()
		survived := reflect.DeepEqual(got, want)

		reason, expected := oneWay[field.Name]

		switch {

		case !survived && !expected:
			t.Errorf("field %s did not survive the round trip: got %v, want %v - either carry it "+
				"through ToProto/FromProto or add it to the one-way list with a reason",
				field.Name, got, want)

		case survived && expected:
			t.Errorf("field %s is listed as one-way (%q) but survived the round trip - remove it "+
				"from the list", field.Name, reason)

		}
	}
}

// populate fills every exported field of a struct with a non-zero value, recursing into nested
// structs and pointers so a field cannot look preserved merely by being left at its zero value.
func populate(t *testing.T, v reflect.Value) {
	t.Helper()

	structType := v.Type()

	for i := range v.NumField() {
		field := structType.Field(i)

		if !field.IsExported() {
			continue
		}

		target := v.Field(i)

		switch target.Kind() {

		case reflect.String:
			target.SetString("x" + field.Name)

		case reflect.Int, reflect.Int32, reflect.Int64:
			target.SetInt(int64(11 + i))

		case reflect.Bool:
			target.SetBool(true)

		case reflect.Map:
			m := reflect.MakeMap(target.Type())
			m.SetMapIndex(reflect.ValueOf("key").Convert(target.Type().Key()),
				reflect.ValueOf("value").Convert(target.Type().Elem()))
			target.Set(m)

		case reflect.Slice:
			element := reflect.New(target.Type().Elem()).Elem()

			if element.Kind() == reflect.Struct {
				populate(t, element)
			} else if element.Kind() == reflect.String {
				element.SetString("element")
			}

			target.Set(reflect.Append(target, element))

		case reflect.Pointer:
			element := reflect.New(target.Type().Elem())

			if element.Elem().Kind() == reflect.Struct {
				populate(t, element.Elem())
			} else if element.Elem().Kind() == reflect.Int64 {
				element.Elem().SetInt(int64(11 + i))
			}

			target.Set(element)

		default:
			t.Fatalf("field %s has kind %s, which populate does not know how to fill - extend it "+
				"rather than letting the guard skip a field", field.Name, target.Kind())

		}
	}
}

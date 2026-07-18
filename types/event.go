package types

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/fastbean-au/hippocampus/contract"
)

type Event struct {
	Id                       string // if not provided, will be a uuid
	TimeStart                int64  // time.Time.Now().UnixNano()
	TimeEnd                  int64  // time.Time.Now().UnixNano()
	Significance             int32
	Name                     string // limited to 256 characters
	Description              string // limited to 1024 characters
	Relationships            []Relationship
	RelationshipSignificance int64  // Sum of the significances of all values of the Relationships. This is a calculated value.
	MemoriesConsolidated     bool   // if true, some memories related to this event have been consolidated (deleted)
	Group                    string // optional grouping/context label; limited to 128 characters

	// SignificanceLevelID is the resolved significance registry level id, set by the RPC layer via
	// db.ResolveSignificanceLevel before a create/update reaches the store. nil means unranked on a
	// create, or "leave significance unchanged" on a partial update. It is internal - never part of
	// the proto conversion.
	SignificanceLevelID *int64
}

type Relationship struct {
	EventId      string
	Significance int32
}

func (r *Relationship) ToProto() *contract.Relationship {
	return &contract.Relationship{
		EventId:      r.EventId,
		Significance: r.Significance,
	}
}

func EventFromProto(event *contract.Event) Event {
	return Event{
		Id:            event.GetId(),
		TimeStart:     event.GetTimeStart(),
		TimeEnd:       event.GetTimeEnd(),
		Significance:  event.GetSignificance(),
		Name:          event.GetName(),
		Description:   event.GetDescription(),
		Relationships: RelationshipsFromProto(event.GetRelationships()),
		Group:         event.GetGroup(),
	}
}

func RelationshipsFromProto(relationships []*contract.Relationship) []Relationship {
	rs := make([]Relationship, len(relationships))

	for i, r := range relationships {
		rs[i] = Relationship{
			EventId:      r.GetEventId(),
			Significance: r.GetSignificance(),
		}
	}

	return rs
}

func (e *Event) ToProto() *contract.Event {
	relationships := make([]*contract.Relationship, len(e.Relationships))
	for i, r := range e.Relationships {
		relationships[i] = r.ToProto()
	}

	return &contract.Event{
		Id:                       e.Id,
		TimeStart:                e.TimeStart,
		TimeEnd:                  e.TimeEnd,
		Significance:             e.Significance,
		Name:                     e.Name,
		Description:              e.Description,
		MemoriesConsolidated:     e.MemoriesConsolidated,
		RelationshipSignificance: e.RelationshipSignificance,
		Relationships:            relationships,
		Group:                    e.Group,
	}
}

func (e *Event) CalculateRelationshipSignificance() int64 {
	var t int64
	for _, r := range e.Relationships {
		t += int64(r.Significance)
	}
	return t
}

func (e *Event) Validate(update bool) error {
	switch {
	case update && len(e.Id) == 0:
		return fmt.Errorf("event not valid - id must be provided")
	case e.Significance < 0:
		// 0 is a valid significance now - it means unranked, an event created (or left) without a
		// place on the significance scale, to be ranked later via UpdateEventSignificance or a
		// placement. Only a negative value is rejected: ranks are non-negative by design.
		return fmt.Errorf("event not valid - significance must not be < 0")
	case len(e.Id) > 128:
		return fmt.Errorf("event not valid - id too long")
	case !update && len(e.Name) == 0:
		return fmt.Errorf("event not valid - no name provided")
	case len(e.Name) > 256:
		return fmt.Errorf("event not valid - name too long")
	case len(e.Description) > 1024:
		return fmt.Errorf("event not valid - description too long")
	case len(e.Group) > 128:
		return fmt.Errorf("event not valid - group too long")
	case !update && e.TimeStart <= 0:
		return fmt.Errorf("event not valid - TimeStart must be > 0")
	case update && e.TimeStart < 0:
		return fmt.Errorf("event not valid - TimeStart must not be < 0")
	case !update && e.TimeEnd < 0:
		return fmt.Errorf("event not valid - TimeEnd must be > 0")
	case update && e.TimeEnd < 0:
		return fmt.Errorf("event not valid - TimeEnd must not be < 0")
	case e.TimeStart > 0 && e.TimeEnd > 0 && e.TimeEnd < e.TimeStart:
		// An event cannot end before it starts. Only checked when both are supplied here; EndEvent
		// updates only time_end and would need the stored time_start to compare -
		// consolidation's age base uses max(time_start, time_end), so a backwards pair there degrades
		// gracefully rather than corrupting.
		return fmt.Errorf("event not valid - TimeEnd must not be before TimeStart")
	// TODO: validate relationships
	default:
		return nil
	}
}

func (e *Event) SetDefaults() {
	if e.Id == "" {
		e.Id = uuid.New().String()
	}

	if e.TimeStart == 0 {
		e.TimeStart = time.Now().UnixNano()
	}
}

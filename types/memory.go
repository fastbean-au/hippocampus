package types

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/fastbean-au/hippocampus/contract"
)

// maxClockSkew is how far ahead of the server clock a memory's timestamp may sit before
// ValidateInsert rejects it as future-dated. A write with a far-future timestamp would have a
// negative age, which makes it undeletable by decay and ranks it last for capacity eviction -
// effectively unforgettable, defeating the storage bound. A few minutes' allowance tolerates
// ordinary client/server clock skew. Imports bypass ValidateInsert (they deliberately carry
// historical timestamps), so this only bounds the fresh-write RPCs.
const maxClockSkew = 5 * time.Minute

type Memory struct {
	Id           string // if not provided, will be a uuid
	TimeStamp    int64  // time.Time.Now().UnixNano()
	Significance int32  // the level's rank on read; on write the requested absolute value (0 = unranked)
	EventId      string
	Body         string // optionally limited "memory.limit.sizeBytes"
	IsBinary     bool
	TimeRecalled int64  // time of the most recent recall; zero if never recalled
	RecallCount  int32  // number of times the memory has been recalled
	IsSummary    bool   // set on the memory created by ReplaceMemoriesWithSummary
	Group        string // optional grouping/context label; limited to 128 characters

	// SignificanceLevelID is the resolved significance registry level id, set by the RPC layer via
	// db.ResolveSignificanceLevel before a create/update reaches the store. nil means unranked on a
	// create, or "leave significance unchanged" on a partial update. It is internal - never part of
	// the proto conversion.
	SignificanceLevelID *int64
}

func MemoryFromProto(memory *contract.Memory) Memory {
	b := memory.GetIsBinary() == contract.Bool_TRUE

	return Memory{
		Id:           memory.GetId(),
		TimeStamp:    memory.GetTimeStamp(),
		Significance: memory.GetSignificance(),
		EventId:      memory.GetEventId(),
		Body:         memory.GetBody(),
		IsBinary:     b,
		TimeRecalled: memory.GetTimeRecalled(),
		RecallCount:  memory.GetRecallCount(),
		IsSummary:    memory.GetIsSummary(),
		Group:        memory.GetGroup(),
	}
}

func (m *Memory) ToProto() *contract.Memory {
	b := contract.Bool_FALSE
	if m.IsBinary {
		b = contract.Bool_TRUE
	}

	return &contract.Memory{
		Id:           m.Id,
		TimeStamp:    m.TimeStamp,
		Significance: m.Significance,
		EventId:      m.EventId,
		Body:         m.Body,
		IsBinary:     b,
		TimeRecalled: m.TimeRecalled,
		RecallCount:  m.RecallCount,
		IsSummary:    m.IsSummary,
		Group:        m.Group,
	}
}

func (m *Memory) ValidateInsert(maxMemoryBodyLength int, update bool) error {
	switch {
	case update && len(m.Id) == 0:
		return fmt.Errorf("memory not valid - id must be provided")
	case m.Significance < 0:
		// 0 is a valid significance now - it means unranked, a memory created (or left) without a
		// place on the significance scale, to be ranked later via UpdateMemory or a placement. Only
		// a negative value is rejected: ranks are non-negative by design.
		return fmt.Errorf("memory not valid - significance must not be < 0")
	case len(m.Id) > 128:
		return fmt.Errorf("memory not valid - id too long")
	case !update && len(m.Body) == 0:
		return fmt.Errorf("memory not valid - no body provided")
	case maxMemoryBodyLength > 0 && len(m.Body) > maxMemoryBodyLength:
		return fmt.Errorf("memory not valid - body too long")
	case len(m.Group) > 128:
		return fmt.Errorf("memory not valid - group too long")
	case m.TimeStamp < 0:
		return fmt.Errorf("memory not valid - timestamp must not be < 0")
	case m.TimeStamp > time.Now().UnixNano()+maxClockSkew.Nanoseconds():
		// Rejected on both the insert and update arms: a future timestamp is equally corrupting
		// whichever RPC sets it (StoreMemory or UpdateMemory), and no legitimate write is future-dated.
		return fmt.Errorf("memory not valid - timestamp is too far in the future")
	default:
		return nil
	}
}

func (m *Memory) SetDefaults() {
	if m.Id == "" {
		m.Id = uuid.New().String()
	}

	if m.TimeStamp == 0 {
		m.TimeStamp = time.Now().UnixNano()
	}
}

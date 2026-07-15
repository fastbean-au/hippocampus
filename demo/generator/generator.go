package main

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/contract"
)

type Config struct {
	Address        string
	DataDirectory  string
	MaxBytes       int64
	Seed           int64
	BurstyWorkers  int
	SlowWorkers    int
	LooseWorkers   int
	QueryWorkers   int
	MutatorWorkers int
}

type Generator struct {
	cfg    Config
	client contract.HippocampusClient
	reg    *registry
	budget *budget
	lat    *latencyTracker

	eventsStored   atomic.Int64
	memoriesStored atomic.Int64
	bytesStored    atomic.Int64
	queriesRun     atomic.Int64
	recallsRun     atomic.Int64
}

func New(cfg Config, client contract.HippocampusClient, lat *latencyTracker) *Generator {
	return &Generator{
		cfg:    cfg,
		client: client,
		reg:    newRegistry(4096, 32768),
		budget: newBudget(cfg.MaxBytes),
		lat:    lat,
	}
}

// Run starts the budget watcher, the statistics logger, and all of the workers, then blocks
// until the context is cancelled and every worker has stopped.
func (g *Generator) Run(ctx context.Context) {
	log.Trace("Run()")

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		g.budget.watch(ctx, g.cfg.DataDirectory)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		g.logStats(ctx)
	}()

	workerSets := []struct {
		count  int
		worker func(context.Context, int)
	}{
		{g.cfg.BurstyWorkers, g.burstyWorker},
		{g.cfg.SlowWorkers, g.slowWorker},
		{g.cfg.LooseWorkers, g.looseWorker},
		{g.cfg.QueryWorkers, g.queryWorker},
		{g.cfg.MutatorWorkers, g.mutatorWorker},
	}

	id := 0

	for _, set := range workerSets {
		for i := 0; i < set.count; i++ {
			id++

			wg.Add(1)
			go func(n int, worker func(context.Context, int)) {
				defer wg.Done()
				worker(ctx, n)
			}(id, set.worker)
		}
	}

	wg.Wait()
}

// burstyWorker idles, then creates an event and floods it with memories in a short burst.
func (g *Generator) burstyWorker(ctx context.Context, n int) {
	log.Tracef("burstyWorker() %d", n)

	rng := rand.New(rand.NewSource(g.cfg.Seed + int64(n)*7919))

	for ctx.Err() == nil {
		if !sleepFor(ctx, randomDuration(rng, 5*time.Second, 45*time.Second)) {
			return
		}

		if g.budget.paused() {
			continue
		}

		g.burst(ctx, rng)
	}
}

func (g *Generator) burst(ctx context.Context, rng *rand.Rand) {
	log.Trace("burst()")

	// Bursts are backdated by up to 30 minutes. The demo config compresses the decay clock
	// (consolidation.unitsOfAgeInDays 0.002 makes one age unit ~3 minutes), so this small jitter
	// spreads the bursts across ~10 age units — old enough for consolidation to start forgetting
	// the less significant ones almost immediately, while the rest decay over the session.
	start := backdatedTimestamp(rng, 30*time.Minute)

	eventID, err := g.storeEvent(ctx, &contract.Event{
		Name:         randomName(rng, "burst"),
		Description:  randomText(rng, 100+rng.Intn(400)),
		Significance: 1 + rng.Int31n(100),
		TimeStart:    start,
	})
	if err != nil {
		return
	}

	count := 20 + rng.Intn(180)
	timestamp := start

	for i := 0; i < count; i++ {
		if ctx.Err() != nil || g.budget.paused() {
			break
		}

		timestamp += int64(randomDuration(rng, 100*time.Millisecond, 30*time.Second))

		g.storeMemory(ctx, rng, &contract.Memory{
			TimeStamp:    timestamp,
			Significance: 1 + rng.Int31n(100),
			EventId:      eventID,
		})

		if !sleepFor(ctx, randomDuration(rng, 5*time.Millisecond, 50*time.Millisecond)) {
			break
		}
	}

	// Most bursts end their event; the rest stay open for the mutator to end or merge later.
	if rng.Intn(10) < 7 {
		g.endEvent(ctx, eventID, timestamp)
	}
}

// slowWorker creates an event that lives for minutes, trickling memories into it before ending
// it and starting the next one.
func (g *Generator) slowWorker(ctx context.Context, n int) {
	log.Tracef("slowWorker() %d", n)

	rng := rand.New(rand.NewSource(g.cfg.Seed + int64(n)*104729))

	for ctx.Err() == nil {
		if !sleepFor(ctx, randomDuration(rng, 5*time.Second, 20*time.Second)) {
			return
		}

		if g.budget.paused() {
			continue
		}

		g.slowEvent(ctx, rng)
	}
}

func (g *Generator) slowEvent(ctx context.Context, rng *rand.Rand) {
	log.Trace("slowEvent()")

	eventID, err := g.storeEvent(ctx, &contract.Event{
		Name:         randomName(rng, "slow"),
		Description:  randomText(rng, 100+rng.Intn(400)),
		Significance: 1 + rng.Int31n(100),
		TimeStart:    time.Now().UnixNano(),
	})
	if err != nil {
		return
	}

	deadline := time.Now().Add(randomDuration(rng, 1*time.Minute, 5*time.Minute))

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}

		if !g.budget.paused() {
			g.storeMemory(ctx, rng, &contract.Memory{
				TimeStamp:    time.Now().UnixNano(),
				Significance: 1 + rng.Int31n(100),
				EventId:      eventID,
			})
		}

		if !sleepFor(ctx, randomDuration(rng, 2*time.Second, 8*time.Second)) {
			return
		}
	}

	g.endEvent(ctx, eventID, time.Now().UnixNano())
}

// looseWorker stores memories with no associated event. They skew less significant and are
// backdated, giving consolidation a steady supply of aged, unattached candidates.
func (g *Generator) looseWorker(ctx context.Context, n int) {
	log.Tracef("looseWorker() %d", n)

	rng := rand.New(rand.NewSource(g.cfg.Seed + int64(n)*1299709))

	for ctx.Err() == nil {
		if !sleepFor(ctx, randomDuration(rng, 1*time.Second, 10*time.Second)) {
			return
		}

		if g.budget.paused() {
			continue
		}

		g.storeMemory(ctx, rng, &contract.Memory{
			TimeStamp:    backdatedTimestamp(rng, 30*time.Minute),
			Significance: 1 + rng.Int31n(50),
		})
	}
}

// queryWorker exercises the read path: range queries over events and memories, lookups by id,
// and recalls (which reinforce the recalled memories against consolidation).
func (g *Generator) queryWorker(ctx context.Context, n int) {
	log.Tracef("queryWorker() %d", n)

	rng := rand.New(rand.NewSource(g.cfg.Seed + int64(n)*15485863))

	for ctx.Err() == nil {
		if !sleepFor(ctx, randomDuration(rng, 2*time.Second, 6*time.Second)) {
			return
		}

		switch rng.Intn(5) {

		case 0:
			g.queryEvents(ctx, rng)

		case 1:
			g.queryMemories(ctx, rng)

		case 2:
			g.queryEventByID(ctx, rng)

		case 3, 4:
			g.recallMemories(ctx, rng)

		}
	}
}

// mutatorWorker exercises the remaining RPCs: significance updates, ending and merging events,
// deletions, and the occasional manual sleep.
func (g *Generator) mutatorWorker(ctx context.Context, n int) {
	log.Tracef("mutatorWorker() %d", n)

	rng := rand.New(rand.NewSource(g.cfg.Seed + int64(n)*32452843))

	for ctx.Err() == nil {
		if !sleepFor(ctx, randomDuration(rng, 15*time.Second, 45*time.Second)) {
			return
		}

		switch rng.Intn(10) {

		case 0, 1, 2:
			g.updateEventSignificance(ctx, rng)

		case 3, 4:
			g.endRandomEvent(ctx, rng)

		case 5:
			g.mergeEvents(ctx, rng)

		case 6:
			g.deleteMemories(ctx, rng)

		case 7:
			g.deleteEvent(ctx, rng)

		case 8:
			g.recallMemories(ctx, rng)

		case 9:
			g.requestSleep(ctx)

		}
	}
}

func (g *Generator) storeEvent(ctx context.Context, event *contract.Event) (string, error) {
	log.Trace("storeEvent()")

	res, err := g.client.StoreEvent(ctx, event)
	if err != nil {
		log.Debugf("StoreEvent failed: %s", err.Error())

		return "", err
	}

	g.eventsStored.Add(1)
	g.reg.addEventID(res.GetId())

	return res.GetId(), nil
}

// storeMemory fills in the memory's body - usually text, occasionally a base64 "binary" blob -
// then stores it and records the id for later queries and recalls.
func (g *Generator) storeMemory(ctx context.Context, rng *rand.Rand, memory *contract.Memory) {
	log.Trace("storeMemory()")

	size := bodySize(rng)

	if rng.Intn(20) == 0 {
		memory.Body = randomBinary(rng, size)
		memory.IsBinary = contract.Bool_TRUE
	} else {
		memory.Body = randomText(rng, size)
		memory.IsBinary = contract.Bool_FALSE
	}

	res, err := g.client.StoreMemory(ctx, memory)
	if err != nil {
		log.Debugf("StoreMemory failed: %s", err.Error())

		return
	}

	g.memoriesStored.Add(1)
	g.bytesStored.Add(int64(len(memory.Body)))
	g.reg.addMemoryIDs([]string{res.GetId()})
}

func (g *Generator) endEvent(ctx context.Context, id string, timeEnd int64) {
	log.Trace("endEvent()")

	if _, err := g.client.EndEvent(ctx, &contract.EndEventRequest{Id: id, TimeEnd: timeEnd}); err != nil {
		log.Debugf("EndEvent failed: %s", err.Error())
	}
}

func (g *Generator) queryEvents(ctx context.Context, rng *rand.Rand) {
	log.Trace("queryEvents()")

	req := &contract.GetEventsRequest{}

	if rng.Intn(2) == 0 {
		req.TimeStartMin = backdatedTimestamp(rng, 2*time.Hour)
		req.TimeStartMax = req.TimeStartMin + int64(randomDuration(rng, 10*time.Minute, time.Hour))
	}

	if rng.Intn(2) == 0 {
		req.SignificanceMin = 1 + rng.Int31n(50)
		req.SignificanceMax = req.SignificanceMin + rng.Int31n(50)
	}

	// Fetching every matching event's memories is expensive; only do it occasionally.
	req.Memories = rng.Intn(10) == 0

	res, err := g.client.GetEvents(ctx, req)
	if err != nil {
		log.Debugf("GetEvents failed: %s", err.Error())

		return
	}

	g.queriesRun.Add(1)

	for _, event := range res.GetEvents() {
		g.reg.addEventID(event.GetId())
	}
}

func (g *Generator) queryMemories(ctx context.Context, rng *rand.Rand) {
	log.Trace("queryMemories()")

	req := &contract.GetMemoriesRequest{
		TimestampMin: backdatedTimestamp(rng, 2*time.Hour),
	}
	req.TimestampMax = req.TimestampMin + int64(randomDuration(rng, 10*time.Minute, time.Hour))

	if rng.Intn(2) == 0 {
		req.SignificanceMin = 1 + rng.Int31n(50)
		req.SignificanceMax = req.SignificanceMin + rng.Int31n(50)
	}

	res, err := g.client.GetMemories(ctx, req)
	if err != nil {
		log.Debugf("GetMemories failed: %s", err.Error())

		return
	}

	g.queriesRun.Add(1)

	ids := make([]string, len(res.GetMemories()))
	for i, memory := range res.GetMemories() {
		ids[i] = memory.GetId()
	}

	g.reg.addMemoryIDs(ids)
}

func (g *Generator) queryEventByID(ctx context.Context, rng *rand.Rand) {
	log.Trace("queryEventByID()")

	id, ok := g.reg.randomEventID(rng)
	if !ok {
		return
	}

	res, err := g.client.GetEventById(ctx, &contract.GetEventByIdRequest{Id: id, Memories: true})
	if err != nil {
		// The event has most likely been consolidated since it was recorded - forget it too.
		g.reg.removeEventID(id)

		return
	}

	g.queriesRun.Add(1)

	memories := res.GetEvent().GetMemories()

	ids := make([]string, len(memories))
	for i, memory := range memories {
		ids[i] = memory.GetId()
	}

	g.reg.addMemoryIDs(ids)
}

func (g *Generator) recallMemories(ctx context.Context, rng *rand.Rand) {
	log.Trace("recallMemories()")

	ids := g.reg.randomMemoryIDs(rng, 1+rng.Intn(10))
	if len(ids) == 0 {
		return
	}

	res, err := g.client.RecallMemories(ctx, &contract.RecallMemoriesRequest{Ids: ids})
	if err != nil {
		log.Debugf("RecallMemories failed: %s", err.Error())

		return
	}

	g.recallsRun.Add(int64(len(res.GetMemories())))

	// Anything requested but not returned has been consolidated - forget it locally as well.
	if len(res.GetMemories()) < len(ids) {
		returned := make(map[string]bool, len(res.GetMemories()))
		for _, memory := range res.GetMemories() {
			returned[memory.GetId()] = true
		}

		gone := make([]string, 0, len(ids))
		for _, id := range ids {
			if !returned[id] {
				gone = append(gone, id)
			}
		}

		g.reg.removeMemoryIDs(gone)
	}
}

func (g *Generator) updateEventSignificance(ctx context.Context, rng *rand.Rand) {
	log.Trace("updateEventSignificance()")

	id, ok := g.reg.randomEventID(rng)
	if !ok {
		return
	}

	req := &contract.UpdateEventSignificanceRequest{
		Id:           id,
		Significance: 1 + rng.Int31n(100),
	}

	if _, err := g.client.UpdateEventSignificance(ctx, req); err != nil {
		log.Debugf("UpdateEventSignificance failed: %s", err.Error())
	}
}

func (g *Generator) endRandomEvent(ctx context.Context, rng *rand.Rand) {
	log.Trace("endRandomEvent()")

	id, ok := g.reg.randomEventID(rng)
	if !ok {
		return
	}

	g.endEvent(ctx, id, time.Now().UnixNano())
}

func (g *Generator) mergeEvents(ctx context.Context, rng *rand.Rand) {
	log.Trace("mergeEvents()")

	to, from, ok := g.reg.twoRandomEventIDs(rng)
	if !ok {
		return
	}

	if _, err := g.client.MergeEvents(ctx, &contract.MergeEventsRequest{MergeTo: to, MergeFrom: from}); err != nil {
		log.Debugf("MergeEvents failed: %s", err.Error())

		return
	}

	g.reg.removeEventID(from)
}

func (g *Generator) deleteMemories(ctx context.Context, rng *rand.Rand) {
	log.Trace("deleteMemories()")

	ids := g.reg.randomMemoryIDs(rng, 1+rng.Intn(5))
	if len(ids) == 0 {
		return
	}

	if _, err := g.client.DeleteMemories(ctx, &contract.DeleteMemoriesRequest{Ids: ids}); err != nil {
		log.Debugf("DeleteMemories failed: %s", err.Error())

		return
	}

	g.reg.removeMemoryIDs(ids)
}

func (g *Generator) deleteEvent(ctx context.Context, rng *rand.Rand) {
	log.Trace("deleteEvent()")

	id, ok := g.reg.randomEventID(rng)
	if !ok {
		return
	}

	// Half the deletions take the event's memories with it; the other half orphan them so the
	// service also sees memories whose event has disappeared.
	req := &contract.DeleteEventRequest{
		Id:       id,
		Memories: rng.Intn(2) == 0,
	}

	if _, err := g.client.DeleteEvent(ctx, req); err != nil {
		log.Debugf("DeleteEvent failed: %s", err.Error())

		return
	}

	g.reg.removeEventID(id)
}

// requestSleep triggers a manual consolidation cycle, which also resets the service's automatic
// sleep timer.
func (g *Generator) requestSleep(ctx context.Context) {
	log.Trace("requestSleep()")

	if _, err := g.client.Sleep(ctx, &contract.EmptyRequest{}); err != nil {
		log.Debugf("Sleep failed: %s", err.Error())
	}
}

func (g *Generator) logStats(ctx context.Context) {
	log.Trace("logStats()")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {

		case <-ctx.Done():
			return

		case <-ticker.C:
			log.WithFields(log.Fields{
				"events_stored":   g.eventsStored.Load(),
				"memories_stored": g.memoriesStored.Load(),
				"bytes_stored":    humanize.Bytes(uint64(g.bytesStored.Load())),
				"database_size":   humanize.Bytes(uint64(g.budget.databaseBytes())),
				"queries":         g.queriesRun.Load(),
				"recalls":         g.recallsRun.Load(),
				"paused":          g.budget.paused(),
			}).Info("generator statistics")

			// One line per RPC class covering just the last interval, so a stall behind a
			// sleep cycle shows in that tick's percentiles rather than averaging away.
			summaries := g.lat.drain()

			for _, class := range latencyClasses {
				s, ok := summaries[class]
				if !ok {
					continue
				}

				log.WithFields(log.Fields{
					"op":    class,
					"count": s.count,
					"p50":   s.p50.Round(time.Microsecond).String(),
					"p95":   s.p95.Round(time.Microsecond).String(),
					"p99":   s.p99.Round(time.Microsecond).String(),
					"max":   s.max.Round(time.Microsecond).String(),
				}).Info("rpc latency")
			}

		}
	}
}

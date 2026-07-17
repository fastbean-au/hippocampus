package main

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/contract"
)

// significanceBands are the [min,max] significance ranges the population sampler buckets memories
// into, so the shape of the surviving population - and how consolidation shifts it upward as it
// forgets the least significant first - is visible over the run. Significance is 1..100.
var significanceBands = [][2]int32{
	{1, 25},
	{26, 50},
	{51, 75},
	{76, 100},
}

// sampleLoop periodically records the shape of the store: total events and memories and the memory
// significance distribution. It reads counts cheaply (TotalCount with a limit of 1), so it barely
// perturbs the workload, and every field is low-cardinality and parseable from the log.
func (g *Generator) sampleLoop(ctx context.Context) {
	log.Trace("sampleLoop()")

	ticker := time.NewTicker(g.cfg.SampleInterval)
	defer ticker.Stop()

	for {
		select {

		case <-ctx.Done():
			return

		case <-ticker.C:
			g.sample(ctx)

		}
	}
}

func (g *Generator) sample(ctx context.Context) {
	memTotal := g.countMemories(ctx, 0, 0)
	evtTotal := g.countEvents(ctx)

	fields := log.Fields{
		"mem_total": memTotal,
		"evt_total": evtTotal,
	}

	// Memories per event: how the burst/slow events fan out, and whether consolidation is thinning
	// members faster than whole events. Guarded against a divide-by-zero on an empty store.
	if evtTotal > 0 {
		fields["mem_per_evt"] = float64(memTotal) / float64(evtTotal)
	}

	for i, band := range significanceBands {
		count := g.countMemories(ctx, band[0], band[1])

		fields[significanceBandKey(i)] = count

		// The share of the surviving population in each band makes the upward shift legible without
		// post-processing; low bands should shrink first as consolidation forgets the least valuable.
		if memTotal > 0 {
			fields[significanceBandKey(i)+"_pct"] = float64(count) / float64(memTotal) * 100
		}
	}

	log.WithFields(fields).Info("population sample")
}

func significanceBandKey(i int) string {
	switch i {

	case 0:
		return "sig_1_25"

	case 1:
		return "sig_26_50"

	case 2:
		return "sig_51_75"

	default:
		return "sig_76_100"

	}
}

// countMemories returns the number of memories matching the significance band (0,0 means no band -
// the whole population), reading only the TotalCount so the page itself stays tiny.
func (g *Generator) countMemories(ctx context.Context, sigMin int32, sigMax int32) int32 {
	req := &contract.GetMemoriesRequest{Limit: 1}
	if sigMin > 0 || sigMax > 0 {
		req.SignificanceMin = sigMin
		req.SignificanceMax = sigMax
	}

	res, err := g.client.GetMemories(ctx, req)
	if err != nil {
		log.Debugf("sample GetMemories failed: %s", err.Error())

		return 0
	}

	return res.GetTotalCount()
}

func (g *Generator) countEvents(ctx context.Context) int32 {
	res, err := g.client.GetEvents(ctx, &contract.GetEventsRequest{Limit: 1})
	if err != nil {
		log.Debugf("sample GetEvents failed: %s", err.Error())

		return 0
	}

	return res.GetTotalCount()
}

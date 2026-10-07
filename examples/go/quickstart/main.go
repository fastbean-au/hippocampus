// Command quickstart is the smallest useful Go program against Hippocampus: store a memory, find it
// by content, read it without reinforcing it, recall it (which does reinforce it), ask where it stands
// against the decay rules, and delete it again.
//
//	go run ./examples/go/quickstart --address localhost:50051
//
// It dials through the dial package, which every integration in this repository uses, so --token and
// the TLS options behave exactly as they do there. CI runs it against the compose stack as a contract
// smoke test (TODO-3 item 172).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/dial"
)

func main() {
	address := flag.String("address", "localhost:50051", "the Hippocampus gRPC address")
	token := flag.String("token", os.Getenv("HIPPOCAMPUS_TOKEN"), "bearer token, when the service has auth on")
	flag.Parse()

	if err := run(*address, *token); err != nil {
		fmt.Fprintln(os.Stderr, "quickstart:", err)
		os.Exit(1)
	}
}

func run(address string, token string) error {
	conn, client, err := dial.Dial(dial.Config{Address: address, Token: token, ClientVersion: "hippocampus-example-go/1"})
	if err != nil {
		return err
	}

	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	identity, err := client.WhoAmI(ctx, &contract.EmptyRequest{})
	if err != nil {
		return fmt.Errorf("WhoAmI: %w", err)
	}

	fmt.Printf("connected to %s (role %q)\n", identity.GetVersion(), identity.GetRole())

	// Significance is the number the decay runs on: higher survives longer.
	stored, err := client.StoreMemory(ctx, &contract.Memory{
		Body:         "the 14:03 deploy of the billing service rolled back cleanly",
		Significance: 50,
		Group:        "examples",
	})
	if err != nil {
		return fmt.Errorf("StoreMemory: %w", err)
	}

	id := stored.GetId()
	fmt.Println("stored", id)

	found, err := client.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "billing rollback", Group: "examples"})
	if err != nil {
		return fmt.Errorf("SearchMemories: %w", err)
	}

	fmt.Printf("search found %d memories\n", len(found.GetMemories()))

	// A read by id does not reinforce; a recall does, resetting the decay clock.
	read, err := client.GetMemories(ctx, &contract.GetMemoriesRequest{Ids: []string{id}})
	if err != nil {
		return fmt.Errorf("GetMemories: %w", err)
	}

	if len(read.GetMemories()) != 1 {
		return fmt.Errorf("reading %s back by id returned %d memories", id, len(read.GetMemories()))
	}

	recalled, err := client.RecallMemories(ctx, &contract.RecallMemoriesRequest{Ids: []string{id}})
	if err != nil {
		return fmt.Errorf("RecallMemories: %w", err)
	}

	fmt.Printf("recalled; recall count is now %d\n", recalled.GetMemories()[0].GetRecallCount())

	explained, err := client.ExplainConsolidation(ctx, &contract.ExplainConsolidationRequest{MemoryIds: []string{id}})
	if err != nil {
		return fmt.Errorf("ExplainConsolidation: %w", err)
	}

	for _, v := range explained.GetValuations() {
		fmt.Printf("value %.4g against a threshold of %.4g; %.1f days until forgotten\n",
			v.GetValue(), explained.GetDeletionThreshold(), v.GetDaysUntilForgotten())
	}

	if _, err := client.DeleteMemories(ctx, &contract.DeleteMemoriesRequest{Ids: []string{id}}); err != nil {
		return fmt.Errorf("DeleteMemories: %w", err)
	}

	fmt.Println("deleted", id)

	return nil
}

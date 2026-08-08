// The generator is a long-running load driver for the hippocampus service. It creates events and
// memories with a mix of access patterns - bursty events that receive a flood of memories at
// once, slow events that accumulate memories over minutes, and loose memories with no event at
// all - while other workers query, recall, merge, and delete. A budget watcher pauses generation
// whenever the on-disk database reaches the configured size limit, and resumes once the sleep
// cycle has consolidated enough data to bring it back down.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/fastbean-au/hippocampus/contract"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		log.Fatalf("generator exited with an error: %s", err.Error())
	}
}

// run is main's body with the process-level concerns lifted out, so a test can drive the whole
// startup path - flag binding, client construction, worker launch and shutdown - and stop it by
// cancelling ctx rather than by signalling the process. main passes context.Background(); the
// signal handling below derives from whatever it is given, so a cancelled parent stops the
// generator exactly as SIGTERM does.
func run(ctx context.Context, args []string) error {
	flags := pflag.NewFlagSet("generator", pflag.ContinueOnError)

	flags.StringP("address", "a", "localhost:8300", "address of the hippocampus gRPC service")
	flags.StringP("data_dir", "d", "./demo/data", "directory holding the hippocampus database files")
	flags.String("log_level", "info", "logging level")
	flags.Int64("max_bytes", 1073741824, "pause generation while the database is at or above this size")
	flags.Int64("seed", 0, "random seed; 0 seeds from the current time")
	flags.Int("bursty_workers", 3, "workers creating events with a burst of memories")
	flags.Int("slow_workers", 4, "workers creating events that accumulate memories over time")
	flags.Int("loose_workers", 2, "workers creating memories with no event")
	flags.Int("query_workers", 3, "workers querying and recalling")
	flags.Int("mutator_workers", 1, "workers updating, merging, and deleting")
	flags.Int64("target_bytes_per_sec", 0, "cap aggregate accepted write throughput to this byte rate; 0 disables the throttle")
	flags.Int("body_bytes", 0, "override the mixed body-size distribution with bodies of ~this many bytes (jittered ±50%); 0 uses the default mix")
	flags.Int("sample_interval_seconds", 0, "record the store's population shape this often; 0 disables the sampler")

	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("failed to parse command line flags: %w", err)
	}

	if err := viper.BindPFlags(flags); err != nil {
		return fmt.Errorf("failed to bind command line flags: %w", err)
	}

	level, err := log.ParseLevel(viper.GetString("log_level"))
	if err != nil {
		return fmt.Errorf("invalid log level '%s': %w", viper.GetString("log_level"), err)
	}
	log.SetLevel(level)

	seed := viper.GetInt64("seed")
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	log.Infof("random seed: %d", seed)

	cfg := Config{
		Address:           viper.GetString("address"),
		DataDirectory:     viper.GetString("data_dir"),
		MaxBytes:          viper.GetInt64("max_bytes"),
		Seed:              seed,
		BurstyWorkers:     viper.GetInt("bursty_workers"),
		SlowWorkers:       viper.GetInt("slow_workers"),
		LooseWorkers:      viper.GetInt("loose_workers"),
		QueryWorkers:      viper.GetInt("query_workers"),
		MutatorWorkers:    viper.GetInt("mutator_workers"),
		TargetBytesPerSec: viper.GetInt64("target_bytes_per_sec"),
		BodyBytes:         viper.GetInt("body_bytes"),
		SampleInterval:    time.Duration(viper.GetInt("sample_interval_seconds")) * time.Second,
	}

	// Every RPC is timed at the client so the statistics include per-class latency percentiles.
	lat := newLatencyTracker()

	conn, err := grpc.NewClient(
		cfg.Address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(unaryLatencyInterceptor(lat)),
	)
	if err != nil {
		return fmt.Errorf("failed to create gRPC client for '%s': %w", cfg.Address, err)
	}
	defer func() { _ = conn.Close() }()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Infof("generator started against %s", cfg.Address)

	generator := New(cfg, contract.NewHippocampusClient(conn), lat)
	generator.Run(ctx)

	log.Info("generator stopped")

	return nil
}

// The generator is a long-running load driver for the hippocampus service. It creates events and
// memories with a mix of access patterns - bursty events that receive a flood of memories at
// once, slow events that accumulate memories over minutes, and loose memories with no event at
// all - while other workers query, recall, merge, and delete. A budget watcher pauses generation
// whenever the on-disk database reaches the configured size limit, and resumes once the sleep
// cycle has consolidated enough data to bring it back down.
package main

import (
	"context"
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
	pflag.StringP("address", "a", "localhost:8300", "address of the hippocampus gRPC service")
	pflag.StringP("data_dir", "d", "./demo/data", "directory holding the hippocampus database files")
	pflag.String("log_level", "info", "logging level")
	pflag.Int64("max_bytes", 1073741824, "pause generation while the database is at or above this size")
	pflag.Int64("seed", 0, "random seed; 0 seeds from the current time")
	pflag.Int("bursty_workers", 3, "workers creating events with a burst of memories")
	pflag.Int("slow_workers", 4, "workers creating events that accumulate memories over time")
	pflag.Int("loose_workers", 2, "workers creating memories with no event")
	pflag.Int("query_workers", 3, "workers querying and recalling")
	pflag.Int("mutator_workers", 1, "workers updating, merging, and deleting")
	pflag.Parse()

	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		log.Panicf("failed to bind command line flags: %s", err.Error())
	}

	level, err := log.ParseLevel(viper.GetString("log_level"))
	if err != nil {
		log.Panicf("invalid log level '%s': %s", viper.GetString("log_level"), err.Error())
	}
	log.SetLevel(level)

	seed := viper.GetInt64("seed")
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	log.Infof("random seed: %d", seed)

	cfg := Config{
		Address:        viper.GetString("address"),
		DataDirectory:  viper.GetString("data_dir"),
		MaxBytes:       viper.GetInt64("max_bytes"),
		Seed:           seed,
		BurstyWorkers:  viper.GetInt("bursty_workers"),
		SlowWorkers:    viper.GetInt("slow_workers"),
		LooseWorkers:   viper.GetInt("loose_workers"),
		QueryWorkers:   viper.GetInt("query_workers"),
		MutatorWorkers: viper.GetInt("mutator_workers"),
	}

	// Every RPC is timed at the client so the statistics include per-class latency percentiles.
	lat := newLatencyTracker()

	conn, err := grpc.NewClient(
		cfg.Address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(unaryLatencyInterceptor(lat)),
	)
	if err != nil {
		log.Panicf("failed to create gRPC client for '%s': %s", cfg.Address, err.Error())
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())

	exit := make(chan os.Signal, 1)
	signal.Notify(exit, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-exit
		log.Info("shutdown signal received - stopping generator")
		cancel()
	}()

	log.Infof("generator started against %s", cfg.Address)

	generator := New(cfg, contract.NewHippocampusClient(conn), lat)
	generator.Run(ctx)

	log.Info("generator stopped")
}

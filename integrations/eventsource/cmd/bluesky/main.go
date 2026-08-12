// Command bluesky bridges the public Bluesky firehose into a Hippocampus instance: every post read
// from Jetstream is stored as a memory, and the engagement that follows it - a like, a repost, a
// reply - reinforces that memory through RecallMemories. Because a memory's id is the post's at://
// URI, that reinforcement needs no state and no lookup. Every post therefore arrives with the same
// significance, and what survives is only what people came back to.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/observability"

	blueskybridge "github.com/fastbean-au/hippocampus/integrations/eventsource/bluesky"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

var version = "dev"

// brokerName identifies this adapter in the telemetry and the probe bodies.
const brokerName = "bluesky"

func main() {
	os.Exit(realMain(os.Args[1:]))
}

// realMain is the testable body of main: it registers flags, installs the signal handler, and runs
// serve, returning a process exit code rather than calling os.Exit itself so tests can drive every
// branch.
func realMain(args []string) int {
	if err := registerFlags(pflag.CommandLine, args); err != nil {
		log.Errorf("failed to register command line flags: %s", err.Error())

		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx); err != nil {
		log.Errorf("bluesky bridge exited with an error: %s", err.Error())

		return 1
	}

	return 0
}

func registerFlags(fs *pflag.FlagSet, args []string) error {
	bridge.RegisterCommonFlags(fs)

	fs.String("jetstream-url", blueskybridge.DefaultURL, "Jetstream /subscribe websocket endpoint")
	fs.StringSlice("collections", []string{
		blueskybridge.CollectionPost,
		blueskybridge.CollectionLike,
		blueskybridge.CollectionRepost,
	}, "record collections to subscribe to (Jetstream wantedCollections; at most 100)")
	fs.StringSlice("dids", nil, "restrict to these repositories (Jetstream wantedDids; empty is the whole network)")
	fs.Int64("cursor", 0, "resume point: a Jetstream sequence number or unix-microsecond timestamp; 0 starts at the live tip")
	fs.String("feed", "",
		"at:// URI of a feed generator to take posts from instead of the firehose (engagement still comes from Jetstream)")
	fs.String("feed-appview", blueskybridge.DefaultAppView, "AppView serving getFeed for --feed")
	fs.Int("feed-poll-seconds", 60, "how often --feed is re-read for new posts")
	fs.Bool("feed-backfill", true, "read the whole feed once at startup so the store is populated immediately")
	fs.Bool("feed-seed-recalls", true,
		"carry a backfilled post's observed engagement across as a damped recall count")
	fs.String("events", blueskybridge.EventsNone,
		"thread modelling: none (every post standalone) or thread (an event per thread root)")
	fs.Bool("recall", true, "reinforce a post's memory when it is liked, reposted, or replied to")
	fs.Int("recall-batch-size", 256, "ids per RecallMemories call (0 issues one RPC per engagement)")
	fs.Int("recall-batch-window-ms", 250, "how long ids are buffered before a partial batch is flushed")
	fs.Bool("honour-deletes", true, "delete a memory when its post is deleted upstream")
	fs.StringSlice("langs", nil, "keep only posts declaring one of these languages (empty keeps all)")
	fs.Int("min-text-bytes", 1, "drop a post whose text is shorter than this")
	fs.Int("root-cache-size", 8192,
		"bounded cache of thread roots known to exist (an optimisation; correctness does not depend on it)")
	fs.Int("read-timeout-seconds", 30, "reconnect if no frame arrives within this window")
	fs.Int("error-backoff-seconds", 1, "seconds to wait before retrying a frame whose store failed")
	fs.Int("max-retries", 3, "frame store attempts before the connection is dropped and replayed from the last cursor")
	fs.Int("reconnect-backoff-seconds", 1, "initial reconnect backoff")
	fs.Int("reconnect-max-backoff-seconds", 30, "ceiling on the exponential reconnect backoff")
	fs.Bool("version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse command line flags: %w", err)
	}

	if err := viper.BindPFlags(fs); err != nil {
		return fmt.Errorf("failed to bind command line flags: %w", err)
	}

	viper.SetEnvPrefix("HIPPOCAMPUS_BLUESKY")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	return nil
}

func serve(ctx context.Context) error {
	if viper.GetBool("version") {
		fmt.Println(version)

		return nil
	}

	level, err := log.ParseLevel(viper.GetString("log-level"))
	if err != nil {
		return fmt.Errorf("invalid log level %q: %w", viper.GetString("log-level"), err)
	}

	log.SetLevel(level)

	return run(ctx)
}

func run(ctx context.Context) error {
	events := viper.GetString("events")

	if events != blueskybridge.EventsNone && events != blueskybridge.EventsThread {
		return fmt.Errorf("--events must be %q or %q, got %q",
			blueskybridge.EventsNone, blueskybridge.EventsThread, events)
	}

	collections := viper.GetStringSlice("collections")

	// Jetstream's own cap. Sending more is a server-side rejection, which is a confusing way to find
	// out about a typo in a long list.
	if len(collections) > 100 {
		return fmt.Errorf("--collections accepts at most 100 entries, got %d", len(collections))
	}

	feed := viper.GetString("feed")

	if feed != "" && !strings.HasPrefix(feed, "at://") {
		return fmt.Errorf("--feed must be an at:// URI, got %q", feed)
	}

	// Not fatal: a bridge subscribed only to likes is a legitimate reinforcement-only deployment, and
	// in feed mode it is the NORMAL one - the feed supplies the posts, so the firehose is wanted only
	// for engagement (and for the delete markers that honour a withdrawal, which do need the post
	// collection).
	if feed == "" && len(collections) > 0 && !slicesContain(collections, blueskybridge.CollectionPost) {
		log.Warnf("--collections does not include %q, so this bridge will store no memories",
			blueskybridge.CollectionPost)
	}

	if feed != "" && viper.GetBool("honour-deletes") && !slicesContain(collections, blueskybridge.CollectionPost) {
		log.Warnf("--collections does not include %q, so upstream deletions of the feed's posts "+
			"will not be honoured", blueskybridge.CollectionPost)
	}

	clientCfg := bridge.ClientConfig{
		Address:               viper.GetString("address"),
		Token:                 viper.GetString("token"),
		TLS:                   viper.GetBool("tls"),
		TLSCACertFile:         viper.GetString("tls-ca-cert"),
		TLSCertFile:           viper.GetString("tls-cert"),
		TLSKeyFile:            viper.GetString("tls-key"),
		TLSInsecureSkipVerify: viper.GetBool("tls-insecure-skip-verify"),
		Endpoint:              "hippocampus",

		// A set client id selects the client-credentials grant over --token; see bridge/oidc.go.
		OIDC: bridge.OIDCConfig{
			Issuer:       viper.GetString("oidc-issuer"),
			TokenURL:     viper.GetString("oidc-token-url"),
			ClientID:     viper.GetString("oidc-client-id"),
			ClientSecret: viper.GetString("oidc-client-secret"),
			Scope:        viper.GetString("oidc-scope"),
			Audience:     viper.GetString("oidc-audience"),
		},
	}

	conn, client, err := bridge.Dial(clientCfg)
	if err != nil {
		return fmt.Errorf("dialling hippocampus: %w", err)
	}

	defer func() { _ = conn.Close() }()

	// Observability and the probe endpoints, started once the connection exists so readiness can
	// report whether the service this bridge writes to can actually serve.
	stopRuntime, err := bridge.StartRuntime(ctx, runtimeConfig(), conn)
	if err != nil {
		return err
	}

	defer stopRuntime()

	transformer := blueskybridge.NewTransformer(transformConfig(), blueskybridge.Options{
		Events:       events,
		Langs:        viper.GetStringSlice("langs"),
		MinTextBytes: viper.GetInt("min-text-bytes"),
	})

	store := bridge.NewStore(client, transformer, time.Duration(viper.GetInt("call-timeout-seconds"))*time.Second, brokerName)

	b := blueskybridge.New(blueskybridge.Config{
		URL:                 viper.GetString("jetstream-url"),
		Collections:         collections,
		DIDs:                viper.GetStringSlice("dids"),
		Cursor:              viper.GetInt64("cursor"),
		Feed:                viper.GetString("feed"),
		FeedAppView:         viper.GetString("feed-appview"),
		FeedPollInterval:    time.Duration(viper.GetInt("feed-poll-seconds")) * time.Second,
		FeedBackfill:        viper.GetBool("feed-backfill"),
		FeedSeedRecalls:     viper.GetBool("feed-seed-recalls"),
		Events:              events,
		Group:               viper.GetString("group"),
		Recall:              viper.GetBool("recall"),
		RecallBatchSize:     viper.GetInt("recall-batch-size"),
		RecallBatchWindow:   time.Duration(viper.GetInt("recall-batch-window-ms")) * time.Millisecond,
		HonourDeletes:       viper.GetBool("honour-deletes"),
		RootCacheSize:       viper.GetInt("root-cache-size"),
		ReadTimeout:         time.Duration(viper.GetInt("read-timeout-seconds")) * time.Second,
		ErrorBackoff:        time.Duration(viper.GetInt("error-backoff-seconds")) * time.Second,
		MaxRetries:          viper.GetInt("max-retries"),
		ReconnectBackoff:    time.Duration(viper.GetInt("reconnect-backoff-seconds")) * time.Second,
		ReconnectMaxBackoff: time.Duration(viper.GetInt("reconnect-max-backoff-seconds")) * time.Second,
	}, store)

	log.WithField("address", clientCfg.Address).Info("connecting to hippocampus")

	return b.Run(ctx)
}

// slicesContain reports whether the slice holds v.
func slicesContain(in []string, v string) bool {
	for _, entry := range in {
		if entry == v {
			return true
		}
	}

	return false
}

func transformConfig() bridge.TransformConfig {
	return bridge.TransformConfig{
		Significance:       viper.GetInt32("significance"),
		SignificanceHeader: viper.GetString("significance-header"),
		Group:              viper.GetString("group"),
		GroupFromSubject:   viper.GetBool("group-from-subject"),
		GroupHeader:        viper.GetString("group-header"),
		Binary:             viper.GetBool("binary"),
		MaxBodyBytes:       viper.GetInt("max-body-bytes"),

		Metadata:             metadataLabels(),
		MetadataHeaders:      viper.GetStringSlice("metadata-header"),
		MetadataHeaderPrefix: viper.GetString("metadata-header-prefix"),
		SubjectMetadataKey:   viper.GetString("subject-metadata-key"),
	}
}

// metadataLabels turns the repeated --metadata 'key=value' flags into the fixed labels stamped on
// every memory, splitting on the FIRST '=' so a value may contain one. A malformed entry is skipped
// with a warning rather than failing startup: the bridge's job is to keep consuming, and one bad
// flag should not stop it.
func metadataLabels() map[string]string {
	raw := viper.GetStringSlice("metadata")
	if len(raw) == 0 {
		return nil
	}

	out := make(map[string]string, len(raw))

	for _, entry := range raw {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			log.Warnf("ignoring malformed --metadata %q (want 'key=value')", entry)

			continue
		}

		out[key] = value
	}

	return out
}

// runtimeConfig assembles the observability wiring from viper. Shared shape across every bridge cmd;
// kept here (not in the bridge package) so all viper reads stay in main.
//
// The tenancy label falls back to --group when --metrics-group is unset, because a bridge configured
// with a fixed group already IS one tenant's - and a resource attribute is the only shape that is
// safe here, since the per-message group can be the message subject and so unbounded.
func runtimeConfig() bridge.RuntimeConfig {
	group := viper.GetString("metrics-group")
	if group == "" {
		group = viper.GetString("group")
	}

	return bridge.RuntimeConfig{
		Broker:  brokerName,
		Version: version,
		Observability: observability.Config{
			TracingEnabled:         viper.GetBool("tracing"),
			TracingSamplingRatio:   viper.GetFloat64("tracing-sampling-ratio"),
			MetricsEnabled:         viper.GetBool("metrics"),
			MetricsIntervalSeconds: viper.GetInt("metrics-interval-seconds"),
			OTLPEndpoint:           viper.GetString("otlp-endpoint"),
			OTLPInsecure:           viper.GetBool("otlp-insecure"),
			Group:                  group,
		},
		HealthPort:        viper.GetInt("health-port"),
		HealthBindAddress: viper.GetString("health-bind-address"),
	}
}

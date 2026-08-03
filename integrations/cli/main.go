// hippo is a command-line client for a running Hippocampus service. It exposes the full RPC surface
// as noun-verb subcommands (memory/event/summary plus the admin and data-movement operations) and
// can talk to the service over either transport: native gRPC (the default) or the JSON/HTTP /v1
// grpc-gateway (--transport http). Both transports share one client interface, so every command
// behaves identically whichever is selected; what a token is actually allowed to do is enforced by
// the service's auth tiers, not by this tool.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// version is stamped into `hippo --version`. It is a var so a release build can override it with
// -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

// execute installs the signal handler, runs the dispatch, and maps the outcome onto a process exit
// code (0 on success, 1 on error, with the error written to stderr). It is split out of main — which
// is reduced to the single os.Exit call it cannot itself be tested through — so the whole
// signal/run/exit-code path is exercised by a test.
func execute(args []string, stdout io.Writer, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, args, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "hippo: %s\n", err.Error())

		return 1
	}

	return 0
}

// run parses args, builds the configured client, and dispatches to the matched command. It is split
// out of main (which only installs the signal handler and maps an error onto the exit code) so the
// whole dispatch path can be exercised by a test with explicit args and output writers.
func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)

		return nil
	}

	switch args[0] {

	case "-h", "--help", "help":
		printUsage(stdout)

		return nil

	case "-v", "--version", "version":
		_, _ = fmt.Fprintln(stdout, version)

		return nil

	case "__complete":
		// Hidden command backing the emitted shell completion scripts; it needs no service
		// connection and its arguments must not be run through the normal flag parser.
		return runComplete(args[1:], stdout)
	}

	key, cmdArgs, cmd, ok := resolveCommand(args)
	if !ok {
		printUsage(stderr)

		return fmt.Errorf("unknown command %q", strings.Join(args, " "))
	}

	fs := pflag.NewFlagSet("hippo "+key, pflag.ContinueOnError)
	fs.SetOutput(stderr)
	registerGlobalFlags(fs)
	cmd.flags(fs)
	fs.Usage = func() { commandUsage(stderr, key, cmd, fs) }

	if err := fs.Parse(cmdArgs); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}

		return err
	}

	cfg, err := configFromFlags(fs)
	if err != nil {
		return err
	}

	if err := applyLogLevel(str(fs, "log-level")); err != nil {
		return err
	}

	client, closeClient, err := newClient(cfg)
	if err != nil {
		return err
	}

	defer func() { _ = closeClient() }()

	if cfg.Timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)

		defer cancel()
	}

	renderer := &renderer{out: stdout, json: str(fs, "output") == "json"}

	return cmd.run(ctx, client, fs, renderer)
}

// resolveCommand locates the subcommand within args and returns its key, the flag arguments to
// parse for it (leading global flags plus everything after the command tokens, with the command
// tokens removed), the matched command, and whether a match was found. Global flags may therefore
// appear on either side of the subcommand.
func resolveCommand(args []string) (string, []string, command, bool) {
	// Probe for the command by parsing only the global flags with interspersing disabled, so
	// parsing stops at the first non-flag token — the start of the command. Unknown flags are
	// whitelisted so a stray flag never aborts the probe.
	probe := pflag.NewFlagSet("probe", pflag.ContinueOnError)
	probe.ParseErrorsAllowlist.UnknownFlags = true
	probe.SetInterspersed(false)
	registerGlobalFlags(probe)

	if err := probe.Parse(args); err != nil {
		return "", nil, command{}, false
	}

	positional := probe.Args()
	if len(positional) == 0 {
		return "", nil, command{}, false
	}

	// The global flags that preceded the command; re-parsed (with the command flags) on the real
	// flag set so their values are captured alongside any trailing flags. Copied into a fresh slice
	// so appending the trailing args never clobbers the shared args backing array.
	leading := args[:len(args)-len(positional)]

	joinArgs := func(trailing []string) []string {
		out := make([]string, 0, len(leading)+len(trailing))
		out = append(out, leading...)

		return append(out, trailing...)
	}

	registry := commands()

	if len(positional) >= 2 {
		if cmd, ok := registry[positional[0]+" "+positional[1]]; ok {
			return positional[0] + " " + positional[1], joinArgs(positional[2:]), cmd, true
		}
	}

	if cmd, ok := registry[positional[0]]; ok {
		return positional[0], joinArgs(positional[1:]), cmd, true
	}

	return "", nil, command{}, false
}

// registerGlobalFlags defines the connection/output flags shared by every command. They are also
// overridable by HIPPOCAMPUS_* environment variables (e.g. HIPPOCAMPUS_TOKEN), resolved in
// configFromFlags.
func registerGlobalFlags(fs *pflag.FlagSet) {
	fs.StringP("transport", "t", "grpc", "transport to the service: 'grpc' or 'http'")
	fs.StringP("address", "a", "", "service address (defaults to localhost:50051 for grpc, localhost:8080 for http)")
	fs.String("token", "", "bearer token sent on every request (overridable by HIPPOCAMPUS_TOKEN)")
	fs.Bool("tls", false, "connect over TLS")
	fs.String("tls-ca-cert", "", "PEM CA bundle to verify the service certificate against (used with --tls)")
	fs.String("tls-cert", "", "client certificate for mutual TLS (used with --tls; requires --tls-key)")
	fs.String("tls-key", "", "client private key for mutual TLS (used with --tls; requires --tls-cert)")
	fs.Bool("tls-insecure-skip-verify", false, "skip verification of the service certificate (dev only; used with --tls)")
	fs.Int("timeout-seconds", 30, "per-request timeout in seconds (0 disables)")
	fs.StringP("output", "o", "text", "output format: 'text' or 'json'")
	fs.String("log-level", "warn", "logging level written to stderr")
}

// configFromFlags resolves the connection configuration from the parsed flags, layering the
// HIPPOCAMPUS_* environment overrides on top via an isolated viper instance. All viper reads for the
// CLI live here, per the repo's main.go convention.
func configFromFlags(fs *pflag.FlagSet) (Config, error) {
	v := viper.New()
	v.SetEnvPrefix("HIPPOCAMPUS")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()

	if err := v.BindPFlags(fs); err != nil {
		return Config{}, fmt.Errorf("failed to bind flags: %w", err)
	}

	transport := v.GetString("transport")

	address := v.GetString("address")
	if address == "" {
		address = "localhost:50051"

		if transport == "http" {
			address = "localhost:8080"
		}
	}

	cfg := Config{
		Transport: transport,
		Address:   address,
		Token:     v.GetString("token"),
		Timeout:   time.Duration(v.GetInt("timeout-seconds")) * time.Second,
		TLS: TLSConfig{
			Enabled:            v.GetBool("tls"),
			CACert:             v.GetString("tls-ca-cert"),
			Cert:               v.GetString("tls-cert"),
			Key:                v.GetString("tls-key"),
			InsecureSkipVerify: v.GetBool("tls-insecure-skip-verify"),
		},
	}

	return cfg, nil
}

// applyLogLevel sets the logrus level (written to stderr) from the --log-level flag.
func applyLogLevel(level string) error {
	parsed, err := log.ParseLevel(level)
	if err != nil {
		return fmt.Errorf("invalid --log-level %q: %w", level, err)
	}

	log.SetLevel(parsed)

	return nil
}

// printUsage writes the top-level help: the invocation form, the global flags, and every command.
// It composes the whole page into a builder and writes it once so a partial write leaves no
// half-rendered help behind.
func printUsage(out io.Writer) {
	var sb strings.Builder

	fmt.Fprintf(&sb, "hippo - command-line client for a Hippocampus service (%s)\n\n", version)
	sb.WriteString("Usage:\n")
	sb.WriteString("  hippo [global flags] <command> [command flags]\n")
	sb.WriteString("  hippo <command> --help\n")
	sb.WriteString("\nCommands:\n")

	registry := commands()

	for _, key := range sortedCommandKeys(registry) {
		fmt.Fprintf(&sb, "  %-22s %s\n", key, registry[key].summary)
	}

	sb.WriteString("\nGlobal flags:\n")

	globals := pflag.NewFlagSet("global", pflag.ContinueOnError)
	registerGlobalFlags(globals)
	sb.WriteString(globals.FlagUsages())

	sb.WriteString("\nEnvironment: any global flag may be set via HIPPOCAMPUS_<FLAG> (e.g. HIPPOCAMPUS_TOKEN).\n")

	_, _ = io.WriteString(out, sb.String())
}

// commandUsage writes the help for a single command: its summary, invocation hint, and its own
// flags (the global flags are documented by the top-level help).
func commandUsage(out io.Writer, key string, cmd command, fs *pflag.FlagSet) {
	var sb strings.Builder

	fmt.Fprintf(&sb, "%s - %s\n\n", key, cmd.summary)
	fmt.Fprintf(&sb, "Usage:\n  hippo [global flags] %s %s\n", key, cmd.hint)

	if local := localFlagUsages(fs); local != "" {
		sb.WriteString("\nCommand flags:\n")
		sb.WriteString(local)
	}

	sb.WriteString("\nRun 'hippo --help' for global flags.\n")

	_, _ = io.WriteString(out, sb.String())
}

// localFlagUsages renders only the command-specific flags, filtering out the global flags shared by
// every command so a command's help stays focused.
func localFlagUsages(fs *pflag.FlagSet) string {
	globals := pflag.NewFlagSet("global", pflag.ContinueOnError)
	registerGlobalFlags(globals)

	local := pflag.NewFlagSet("local", pflag.ContinueOnError)

	fs.VisitAll(func(flag *pflag.Flag) {
		if globals.Lookup(flag.Name) == nil {
			local.AddFlag(flag)
		}
	})

	return local.FlagUsages()
}

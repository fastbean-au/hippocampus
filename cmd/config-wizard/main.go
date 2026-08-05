// Command hippocampus-config-wizard serves the configuration and deployment wizard: a
// browser-based, guided builder for a Hippocampus config.json and the deployment artefacts that
// carry it (Docker Compose, Kubernetes, systemd, or a plain binary run).
//
// It is a static server and nothing more. The wizard is a self-contained single-page application
// that does all of its work in the browser - it never talks back to this process, and it never
// talks to a Hippocampus instance - so the same asset bundle can be run locally with this binary,
// baked into a container image, or dropped behind any static web server. That is deliberate: the
// generated config carries secrets (signing secrets, DSNs, passwords), and none of them should
// ever leave the operator's browser.
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// version is the build identification, stamped at link time with
// -ldflags "-X main.version=<tag>" by the release workflow and the Dockerfile. It matches the
// convention every other tool in this repo uses (the service binary alone reads main.buildVersion).
var version = "dev"

// wizardAssets is the wizard single-page application: markup, stylesheet, and script. They are
// separate files rather than one inlined document (as the service's own /ui console is) so the
// page can be served under a strict Content-Security-Policy with no 'unsafe-inline' - this one is
// published on the public internet, where the console is not.
//
//go:embed wizard
var wizardAssets embed.FS

func main() {
	if err := execute(os.Args[1:]); err != nil {
		log.Fatalf("hippocampus-config-wizard: %s", err.Error())
	}
}

// execute is main() with its process-owned inputs made injectable: it takes the argument slice
// rather than reading os.Args and returns an error rather than exiting, so a test can drive the
// whole startup path in process. Every viper read lives here, per the repo's convention that
// configuration is read only in a program's main package entrypoint.
func execute(args []string) error {
	flags := pflag.NewFlagSet("hippocampus-config-wizard", pflag.ContinueOnError)
	flags.Int("port", 8091, "HTTP listen port")
	flags.String("bind-address", "", "interface to bind (empty, the default, binds all interfaces; 127.0.0.1 restricts to loopback)")
	flags.String("log-level", "info", "minimum log severity: trace, debug, info, warn, error")
	flags.Bool("version", false, "print the build version and exit")

	// --help is not an error: pflag prints usage and returns ErrHelp, which should exit cleanly.
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}

		return fmt.Errorf("failed to parse command line flags: %w", err)
	}

	// A local viper rather than the global one: this program has no config file, only flags and
	// HIPPOCAMPUS_WIZARD_* environment overrides, and a package-level instance would leak state
	// between tests.
	v := viper.New()

	if err := v.BindPFlags(flags); err != nil {
		return fmt.Errorf("failed to bind command line flags: %w", err)
	}

	v.SetEnvPrefix("HIPPOCAMPUS_WIZARD")
	v.AutomaticEnv()

	if v.GetBool("version") {
		fmt.Println(version)

		return nil
	}

	initLogging(v.GetString("log-level"))

	address := net.JoinHostPort(v.GetString("bind-address"), strconv.Itoa(v.GetInt("port")))

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", address, err)
	}

	server := &http.Server{
		Handler: handler(),

		// The wizard is a handful of static files with no request body worth reading, so the
		// timeouts can be tight; they exist to stop a slow-loris client holding a connection open.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Infof("configuration wizard listening on http://%s/ (version %s)", listener.Addr().String(), version)

	return serve(server, listener)
}

// serve runs the server until the process is signalled, then drains it. It is split from execute so
// a test can exercise the shutdown path with its own listener.
func serve(server *http.Server, listener net.Listener) error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	defer signal.Stop(signals)

	errs := make(chan error, 1)

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err

			return
		}

		errs <- nil
	}()

	select {

	case err := <-errs:
		return err

	case sig := <-signals:
		log.Infof("received %s, shutting down", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shut down cleanly: %w", err)
	}

	return <-errs
}

// handler builds the whole HTTP surface: the embedded wizard assets, plus a /healthz endpoint for
// container and load-balancer probes.
func handler() http.Handler {
	assets, err := fs.Sub(wizardAssets, "wizard")
	if err != nil {
		// Unreachable: the directory is embedded at compile time, so it is always present.
		log.Panicf("failed to open the embedded wizard assets: %s", err.Error())
	}

	mux := http.NewServeMux()
	mux.Handle("/healthz", healthHandler())
	mux.Handle("/", securityHeaders(http.FileServer(http.FS(assets))))

	return mux
}

// healthHandler reports process liveness. There is no dependency to check - the wizard is static -
// so it answers unconditionally, carrying the build version so an operator can tell what is
// deployed (mirroring the service's own /healthz body).
func healthHandler() http.Handler {
	body := []byte(`{"status":"ok","version":"` + version + `"}`)

	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		_, _ = w.Write(body)
	})
}

// securityHeaders wraps the static file server with the response headers a public deployment
// wants. The Content-Security-Policy is the important one: the page loads only same-origin CSS and
// JS, embeds its icon as an inline SVG data URI, and makes no network requests at all, so
// everything else - including connections back to any origin - can be denied outright. Keep this
// in step with the markup: adding an external font, an inline <script>, or any fetch() would need
// a matching relaxation here, or the browser will silently block it.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self' data:; "+
				"connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		// The assets are not content-hashed and change with every build, so never serve a cached
		// copy without revalidating - otherwise a stale wizard lingers after an upgrade.
		w.Header().Set("Cache-Control", "no-cache")

		next.ServeHTTP(w, r)
	})
}

// initLogging configures logrus to match the rest of the repo: human-readable text on stdout at the
// requested severity, falling back to info for an unset or unrecognised level.
func initLogging(level string) {
	parsed, err := log.ParseLevel(level)
	if err != nil {
		parsed = log.InfoLevel
	}

	log.SetOutput(os.Stdout)
	log.SetLevel(parsed)
}

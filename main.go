// Command hugin-stitch-gui is a local web UI for panorama stitching backed by
// the Hugin command-line tools.
//
// Run it, open http://127.0.0.1:8765, drop images in, and click "Stitch".
//
// Pipeline (all external tools, none bundled):
//
//	pto_gen -> cpfind -> autooptimiser -> pano_modify -> hugin_executor
//
// hugin_executor remaps with nona and blends with enblend (GPU-accelerated via
// OpenCL when enblend was built with it).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultPort = 8765

// version is what this build calls itself. Releases stamp it with the tag they
// were built from, via -ldflags "-X main.version=v1.2.3"; anything built
// straight from a checkout stays "dev".
var version = "dev"

func main() {
	log.SetFlags(0)
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		hostFlag    = flag.String("host", "", "address to bind (default 127.0.0.1, or $HOST)")
		portFlag    = flag.Int("port", 0, "port to listen on (default 8765, or $PORT)")
		staticFlag  = flag.String("static", "", "serve the UI from this directory instead of the embedded copy")
		huginFlag   = flag.String("hugin-dir", "", "directory holding the Hugin tools, if they are somewhere unusual (or $HUGIN_DIR)")
		keepFlag    = flag.Duration("retention", 0, "how long to keep a finished stitch after the last time the page asked for it, e.g. 30m (default 2h, or $RETENTION; 0s keeps them until the server stops)")
		versionFlag = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = usage
	flag.Parse()

	if *versionFlag {
		fmt.Println("hugin-stitch-gui " + version)
		return nil
	}

	port, err := resolvePort(*portFlag, flag.Args())
	if err != nil {
		return err
	}
	retention, err := resolveRetention(*keepFlag, flagPassed("retention"))
	if err != nil {
		return err
	}
	host := *hostFlag
	if host == "" {
		host = envOr("HOST", "127.0.0.1")
	}

	tools := DetectToolchain(huginDirs(*huginFlag)...)
	reportToolchain(tools)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := NewServer(ctx, tools, *staticFlag)
	if err != nil {
		return fmt.Errorf("could not load the UI: %w", err)
	}

	// Clear anything left behind by a run that was killed before it could
	// tidy up. Directories belonging to another running server are left alone.
	for _, dir := range sweepOrphans(os.TempDir(), time.Now(), orphanAge) {
		fmt.Println("Removed leftover work directory " + dir)
	}
	go server.Retain(ctx, retention)

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Handler: server.Handler(),
		// Stitching a large panorama can take many minutes, and the browser
		// holds the upload connection open for the whole transfer, so neither
		// direction can carry a short deadline.
		ReadHeaderTimeout: 30 * time.Second,
	}

	fmt.Printf("Serving Hugin Stitch UI at http://%s  (Ctrl+C to stop)\n", addr)

	errCh := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		fmt.Println("\nStopped.")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := httpServer.Shutdown(shutdownCtx)
		// Shutdown only waits for HTTP handlers. The stitch itself runs on its
		// own goroutine, so the work directories are cleared after it stops.
		server.Cleanup(5 * time.Second)
		return err
	}
}

// resolvePort honours, in order: the -port flag, a positional argument (so
// `hugin-stitch-gui 9000` keeps working), $PORT, then the default.
func resolvePort(flagValue int, args []string) (int, error) {
	if flagValue != 0 {
		return flagValue, nil
	}
	if len(args) > 0 {
		port, err := strconv.Atoi(args[0])
		if err != nil {
			return 0, fmt.Errorf("invalid port %q", args[0])
		}
		return port, nil
	}
	if raw := os.Getenv("PORT"); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil {
			return 0, fmt.Errorf("invalid PORT %q", raw)
		}
		return port, nil
	}
	return defaultPort, nil
}

// resolveRetention honours the -retention flag, then $RETENTION, then the
// default. The flag is read as "was it given" rather than "is it non-zero",
// so -retention 0s can ask for no expiry at all.
func resolveRetention(flagValue time.Duration, given bool) (time.Duration, error) {
	if given {
		if flagValue < 0 {
			return 0, fmt.Errorf("invalid retention %v", flagValue)
		}
		return flagValue, nil
	}
	if raw := os.Getenv("RETENTION"); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("invalid RETENTION %q", raw)
		}
		return value, nil
	}
	return DefaultRetention, nil
}

// flagPassed reports whether a flag was actually given on the command line,
// which a zero value cannot tell us on its own.
func flagPassed(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// huginDirs returns the directories named by the -hugin-dir flag or the
// HUGIN_DIR environment variable. Several can be given, separated the way the
// platform separates PATH entries.
func huginDirs(flagValue string) []string {
	value := flagValue
	if value == "" {
		value = os.Getenv("HUGIN_DIR")
	}
	if value == "" {
		return nil
	}
	var dirs []string
	for _, dir := range filepath.SplitList(value) {
		if dir = strings.TrimSpace(dir); dir != "" {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// reportToolchain prints what was found and, when something is missing, how to
// install it on this platform.
func reportToolchain(tools Toolchain) {
	if !tools.Ready {
		fmt.Fprintln(os.Stderr, "WARNING: "+tools.MissingMessage())
		fmt.Fprintln(os.Stderr, "If Hugin is installed somewhere unusual, point at it with -hugin-dir.")
	}
	if tools.RawConverter == "" {
		fmt.Fprintln(os.Stderr, "Note: no RAW decoder found, so CR2/CR3/NEF/ARW/DNG uploads will be skipped.")
	}
	if tools.DecoderFor(familyHEIF) == "" {
		fmt.Fprintln(os.Stderr, "Note: no HEIC decoder found, so HEIC/HEIF uploads will be skipped.")
	}
	if !tools.Has("exiftool") {
		fmt.Fprintln(os.Stderr, "Note: exiftool not found, so panoramas will not keep their capture time.")
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintf(out, "Usage: %s [flags] [port]\n\n", os.Args[0])
	fmt.Fprintln(out, "A local web UI for stitching panoramas with the Hugin command-line tools.")
	fmt.Fprintln(out, "Version "+version+".")
	fmt.Fprintln(out, "\nFlags:")
	flag.PrintDefaults()
}

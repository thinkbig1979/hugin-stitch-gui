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
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultPort = 8765

func main() {
	log.SetFlags(0)
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		hostFlag   = flag.String("host", "", "address to bind (default 127.0.0.1, or $HOST)")
		portFlag   = flag.Int("port", 0, "port to listen on (default 8765, or $PORT)")
		staticFlag = flag.String("static", "", "serve the UI from this directory instead of the embedded copy")
	)
	flag.Usage = usage
	flag.Parse()

	port, err := resolvePort(*portFlag, flag.Args())
	if err != nil {
		return err
	}
	host := *hostFlag
	if host == "" {
		host = envOr("HOST", "127.0.0.1")
	}

	tools := DetectToolchain()
	if !tools.Ready {
		fmt.Fprintln(os.Stderr, "WARNING: Hugin CLI tools missing:", strings.Join(tools.Missing, ", "))
		fmt.Fprintln(os.Stderr, "Install with: sudo apt install hugin-tools enblend")
	}
	if tools.RawConverter == "" {
		fmt.Fprintln(os.Stderr, "Note: no RAW decoder found; CR2/CR3/NEF/ARW/DNG uploads will be skipped.")
		fmt.Fprintln(os.Stderr, "Install one with: sudo apt install libraw-bin")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := NewServer(ctx, tools, *staticFlag)
	if err != nil {
		return fmt.Errorf("could not load the UI: %w", err)
	}

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
		return httpServer.Shutdown(shutdownCtx)
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
	fmt.Fprintln(out, "\nFlags:")
	flag.PrintDefaults()
}

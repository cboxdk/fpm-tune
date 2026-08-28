// Command loadgen puts sustained, controlled concurrency on a PHP-FPM pool.
//
// It exists because verifying fpm-tune needs a pool that is genuinely saturated,
// and the obvious way to arrange that does not work. A shell loop spawning
// cgi-fcgi processes serialises on process creation: six "concurrent" requests
// produced a measured max_active_processes of 2, so the saturation path could
// not be exercised at all and the tool looked like it was cutting a busy pool
// when it was reading an idle one.
//
// Goroutines over persistent FastCGI connections hold real concurrency instead,
// using the same client the tool itself scrapes with.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cboxdk/fcgx"
)

func main() {
	var (
		socket      = flag.String("socket", "", "unix socket or host:port of the pool")
		script      = flag.String("script", "/var/www/work.php", "SCRIPT_FILENAME to request")
		concurrency = flag.Int("concurrency", 10, "requests in flight at once")
		duration    = flag.Duration("duration", 60*time.Second, "how long to sustain the load")
		query       = flag.String("query", "mb=8&hold=0.2", "QUERY_STRING passed to the script")
	)
	flag.Parse()

	if *socket == "" {
		fmt.Fprintln(os.Stderr, "loadgen: --socket is required")
		os.Exit(2)
	}

	scheme, address := "unix", *socket
	if len(*socket) > 0 && (*socket)[0] != '/' {
		scheme = "tcp"
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *duration)
	defer cancelTimeout()

	var ok, failed atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for ctx.Err() == nil {
				if request(ctx, scheme, address, *script, *query) {
					ok.Add(1)
				} else {
					failed.Add(1)
				}
			}
		}()
	}

	// Progress while it runs, so a run that is not producing load is visible
	// immediately rather than after it finishes.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fmt.Printf("  %d ok, %d failed\n", ok.Load(), failed.Load())
			}
		}
	}()

	wg.Wait()
	fmt.Printf("done: %d ok, %d failed\n", ok.Load(), failed.Load())
}

// request performs one FastCGI request, reporting only whether it succeeded.
// The body is drained and closed so the connection is not left half-read.
func request(ctx context.Context, scheme, address, script, query string) bool {
	client, err := fcgx.DialContext(ctx, scheme, address)
	if err != nil {
		return false
	}
	defer func() { _ = client.Close() }()

	resp, err := client.Get(ctx, map[string]string{
		"SCRIPT_FILENAME": script,
		"SCRIPT_NAME":     "/work.php",
		"REQUEST_METHOD":  "GET",
		"SERVER_SOFTWARE": "fpm-tune-loadgen",
		"REMOTE_ADDR":     "127.0.0.1",
		"QUERY_STRING":    query,
	})
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	if _, err := fcgx.ReadBody(resp); err != nil {
		return false
	}

	return true
}

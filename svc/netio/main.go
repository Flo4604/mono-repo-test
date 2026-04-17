// netio is a test service for exercising heimdall's network metering.
//
// Endpoints:
//   GET /                   info
//   GET /healthz            readiness probe
//   POST /healthz           same
//   GET /net/info           local hostname, env, dest defaults
//   GET /net/public?mb=N    download N MiB from a public URL (drives egress + ingress)
//   GET /net/private?mb=N&host=H&path=P    call an in-cluster service
//                                          (drives private egress + private ingress)
//   GET /net/sink?mb=N      server streams N MiB of zeros to the caller
//                           (drives whatever direction the *caller* counts as)
//
// All sizes are in MiB. The bytes go to /dev/null on either side, so this
// is purely a network exercise. Each endpoint reports the actual transfer
// size and the elapsed time so you can sanity-check what the dashboard
// shows against what the service actually moved.
package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/unkeyed/mono-repo-test/pkg/shared"
)

// Hetzner publishes fixed-size speed-test files at predictable URLs with
// no rate limit and no cap. We pick the smallest one that satisfies the
// requested mb. httpbin caps at 100 KiB; Cloudflare's /__down 403's on
// anything sizable. Hetzner Just Works.
var hetznerSizes = []struct {
	mb  int
	url string
}{
	{1, "https://speed.hetzner.de/1MB.bin"},
	{10, "https://speed.hetzner.de/10MB.bin"},
	{100, "https://speed.hetzner.de/100MB.bin"},
	{1024, "https://speed.hetzner.de/1GB.bin"},
}

// pickPublicURL returns the URL whose declared size is the smallest one
// >= the requested mb. Larger requests download the largest available
// (1 GB) and the response reports what was actually pulled.
func pickPublicURL(mb int) string {
	for _, s := range hetznerSizes {
		if s.mb >= mb {
			return s.url
		}
	}
	return hetznerSizes[len(hetznerSizes)-1].url
}

// Default in-cluster destination when /net/private is called without a host.
// Krane's /health endpoint always returns ~30 bytes; the loop below makes
// repeated requests until the desired byte budget is exhausted.
const (
	defaultPrivateHost = "krane.unkey.svc.cluster.local:8070"
	defaultPrivatePath = "/health"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3459"
	}

	var ready atomic.Bool
	startupDelay := 2 * time.Second
	log.Printf("netio: starting up, will be ready in %s", startupDelay)
	go func() {
		time.Sleep(startupDelay)
		ready.Store(true)
		log.Println("netio: ready to serve traffic")
	}()

	var inflight atomic.Int64

	// Graceful shutdown waits up to 10s for in-flight requests so a long
	// /net/public download isn't truncated when the pod gets evicted.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	go func() {
		s := <-sig
		log.Printf("netio: received %s — starting graceful shutdown", s)
		deadline := time.After(10 * time.Second)
		for inflight.Load() > 0 {
			select {
			case <-deadline:
				log.Printf("netio: shutdown deadline hit with %d in-flight", inflight.Load())
				os.Exit(1)
			default:
				time.Sleep(100 * time.Millisecond)
			}
		}
		log.Printf("netio: clean shutdown after %s", s)
		os.Exit(0)
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "netio",
			Status:  "ok",
			Port:    port,
			Message: "endpoints: /net/info /net/public?mb=N /net/private?mb=N /net/sink?mb=N",
		})
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			shared.JSON(w, http.StatusServiceUnavailable, shared.Response{
				Service: "netio", Status: "not_ready", Port: port,
			})
			return
		}
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "netio", Status: "healthy", Port: port,
		})
	})
	mux.HandleFunc("POST /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			shared.JSON(w, http.StatusServiceUnavailable, shared.Response{
				Service: "netio", Status: "not_ready", Port: port,
			})
			return
		}
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "netio", Status: "healthy", Port: port,
		})
	})

	// /net/info — what the service knows about its own network identity.
	mux.HandleFunc("GET /net/info", func(w http.ResponseWriter, r *http.Request) {
		host, _ := os.Hostname()
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "netio",
			Status:  "ok",
			Port:    port,
			Message: fmt.Sprintf("host=%s instance=%s region=%s default_public=%s default_private=http://%s%s",
				host,
				os.Getenv("UNKEY_INSTANCE_ID"),
				os.Getenv("UNKEY_REGION"),
				defaultPublicURL,
				defaultPrivateHost, defaultPrivatePath,
			),
		})
	})

	// /net/public — drive public egress (request body upload is small;
	// dominant traffic is the response body coming back, which is ingress
	// for this pod and bills against network_ingress_public_bytes).
	mux.HandleFunc("GET /net/public", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		mb := parseMB(r, 5)
		urlBase := r.URL.Query().Get("url")
		if urlBase == "" {
			urlBase = defaultPublicURL
		}
		// Cloudflare uses ?bytes=N; everything else (httpbin, etc.) uses
		// /bytes/N. Heuristic: if the URL already has a query string OR
		// it's the Cloudflare endpoint, append ?bytes=N. Otherwise treat
		// it as a path-style sized download.
		bytes := mb * 1024 * 1024
		var url string
		if urlBase == defaultPublicURL || strings.Contains(urlBase, "?") {
			sep := "?"
			if strings.Contains(urlBase, "?") {
				sep = "&"
			}
			url = fmt.Sprintf("%s%sbytes=%d", urlBase, sep, bytes)
		} else {
			url = fmt.Sprintf("%s/%d", urlBase, bytes)
		}

		log.Printf("netio: GET %s", url)
		start := time.Now()
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			shared.JSON(w, http.StatusBadGateway, shared.Response{
				Service: "netio", Status: "error", Port: port,
				Message: fmt.Sprintf("public GET failed: %v", err),
			})
			return
		}
		defer resp.Body.Close()
		n, err := io.Copy(io.Discard, resp.Body)
		dur := time.Since(start)
		if err != nil {
			shared.JSON(w, http.StatusOK, shared.Response{
				Service: "netio", Status: "partial", Port: port,
				Message: fmt.Sprintf("read %d bytes in %s before error: %v", n, dur, err),
			})
			return
		}
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "netio", Status: "ok", Port: port,
			Message: fmt.Sprintf("downloaded %d bytes (%d MiB) from %s in %s (%.1f MiB/s)",
				n, n/1024/1024, url, dur.Round(time.Millisecond), float64(n)/(1024*1024)/dur.Seconds()),
		})
	})

	// /net/private — drive private egress. Calls an in-cluster endpoint
	// in a loop until the byte budget is hit. Each /health response is
	// small so this issues many requests; both the requests (egress) and
	// responses (ingress) bill against the *_private_bytes counters.
	mux.HandleFunc("GET /net/private", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		mb := parseMB(r, 1)
		host := r.URL.Query().Get("host")
		if host == "" {
			host = defaultPrivateHost
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			path = defaultPrivatePath
		}
		url := fmt.Sprintf("http://%s%s", host, path)
		budget := int64(mb) * 1024 * 1024

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		log.Printf("netio: looping GET %s until %d bytes drained", url, budget)
		start := time.Now()
		var total int64
		var calls int
		for total < budget {
			if ctx.Err() != nil {
				break
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				shared.JSON(w, http.StatusBadGateway, shared.Response{
					Service: "netio", Status: "error", Port: port,
					Message: fmt.Sprintf("private GET failed after %d calls / %d bytes: %v", calls, total, err),
				})
				return
			}
			n, _ := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			total += n
			calls++
		}
		dur := time.Since(start)
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "netio", Status: "ok", Port: port,
			Message: fmt.Sprintf("drained %d bytes via %d calls to %s in %s (%.1f KiB/s)",
				total, calls, url, dur.Round(time.Millisecond), float64(total)/1024/dur.Seconds()),
		})
	})

	// /net/sink — server emits N MiB of zeros. The caller pulls this, so
	// it bills against THIS pod's egress. Useful for the inverse direction
	// (you curl it, your egress chart from this pod's perspective grows).
	mux.HandleFunc("GET /net/sink", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		mb := parseMB(r, 5)
		size := int64(mb) * 1024 * 1024

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)

		start := time.Now()
		n, err := io.CopyN(w, rand.Reader, size)
		dur := time.Since(start)
		log.Printf("netio: sink served %d bytes in %s (err=%v)", n, dur, err)
	})

	log.Printf("netio: listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

// parseMB extracts ?mb=N (default `def`, capped at 1024 MiB).
func parseMB(r *http.Request, def int) int {
	s := r.URL.Query().Get("mb")
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	if n > 1024 {
		n = 1024
	}
	return n
}

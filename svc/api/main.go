package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/unkeyed/mono-repo-test/pkg/shared"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3456"
	}

	// Simulate startup delay — healthcheck should fail during this window
	var ready atomic.Bool
	startupDelay := 3 * time.Second
	log.Printf("api: starting up, will be ready in %s", startupDelay)
	go func() {
		time.Sleep(startupDelay)
		ready.Store(true)
		log.Println("api: ready to serve traffic")
	}()

	// Toggle health on/off via POST /healthz/fail and POST /healthz/recover
	var forceFail atomic.Bool

	// Track in-flight requests for graceful shutdown
	var inflight atomic.Int64

	// Handle shutdown signals — log exactly which signal we got
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGKILL)
	go func() {
		s := <-sig
		log.Printf("api: received %s — starting graceful shutdown", s)

		// Wait for in-flight requests to drain
		deadline := time.After(10 * time.Second)
		for inflight.Load() > 0 {
			select {
			case <-deadline:
				log.Printf("api: shutdown deadline reached with %d in-flight requests", inflight.Load())
				os.Exit(1)
			default:
				time.Sleep(100 * time.Millisecond)
			}
		}

		log.Printf("api: clean shutdown after %s", s)
		os.Exit(0)
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)

		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "api",
			Status:  "ok",
			Port:    port,
			Message: fmt.Sprintf("request #%d | in-flight: %d", rand.Intn(10000), inflight.Load()),
		})
	})

	// Healthcheck endpoint — fails during startup and when toggled
	healthzHandler := func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			shared.JSON(w, http.StatusServiceUnavailable, shared.Response{
				Service: "api",
				Status:  "not_ready",
				Port:    port,
				Message: "still starting up",
			})
			return
		}
		if forceFail.Load() {
			shared.JSON(w, http.StatusServiceUnavailable, shared.Response{
				Service: "api",
				Status:  "unhealthy",
				Port:    port,
				Message: "health manually toggled to fail",
			})
			return
		}
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "api",
			Status:  "healthy",
			Port:    port,
		})
	}
	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.HandleFunc("POST /healthz", healthzHandler)

	// POST /healthz/fail — make healthcheck start failing (triggers liveness probe restart)
	mux.HandleFunc("POST /healthz/fail", func(w http.ResponseWriter, r *http.Request) {
		forceFail.Store(true)
		log.Println("api: healthcheck toggled to FAIL")
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "api",
			Status:  "ok",
			Port:    port,
			Message: "healthcheck will now fail — liveness probe should restart this container",
		})
	})

	// POST /healthz/recover — make healthcheck pass again
	mux.HandleFunc("POST /healthz/recover", func(w http.ResponseWriter, r *http.Request) {
		forceFail.Store(false)
		log.Println("api: healthcheck toggled to PASS")
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "api",
			Status:  "ok",
			Port:    port,
			Message: "healthcheck will now pass again",
		})
	})

	// GET /slow — simulate a slow request (useful for testing graceful shutdown)
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)

		duration := 5 * time.Second
		log.Printf("api: slow request started, will take %s", duration)
		time.Sleep(duration)
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "api",
			Status:  "ok",
			Port:    port,
			Message: fmt.Sprintf("slow request completed after %s", duration),
		})
	})

	// GET /protected — requires X-Unkey-Principal header from sentinel KeyAuth
	mux.HandleFunc("GET /protected", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)

		principal := r.Header.Get("X-Unkey-Principal")
		if principal == "" {
			log.Println("api: GET /protected — no X-Unkey-Principal header")
			shared.JSON(w, http.StatusUnauthorized, shared.Response{
				Service: "api",
				Status:  "unauthorized",
				Port:    port,
				Message: "missing X-Unkey-Principal header — sentinel middleware not configured?",
			})
			return
		}

		log.Printf("api: GET /protected — principal: %s", principal)

		// Parse and echo back the principal
		var parsed map[string]any
		if err := json.Unmarshal([]byte(principal), &parsed); err != nil {
			log.Printf("api: GET /protected — failed to parse principal JSON: %v", err)
			shared.JSON(w, http.StatusOK, shared.Response{
				Service: "api",
				Status:  "ok",
				Port:    port,
				Message: fmt.Sprintf("principal (raw): %s", principal),
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{
			"service":   "api",
			"status":    "authenticated",
			"port":      port,
			"principal": parsed,
		})
	})

	// GET /env — dump all environment variables as pretty JSON
	mux.HandleFunc("GET /env", func(w http.ResponseWriter, r *http.Request) {
		envs := os.Environ()
		sort.Strings(envs)

		vars := make(map[string]string, len(envs))
		for _, e := range envs {
			k, v, _ := strings.Cut(e, "=")
			vars[k] = v
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(vars)
	})

	// GET /probe — try to reach the worker service to test network isolation.
	// Set WORKER_URL env var to the worker's internal address.
	// If apps are properly isolated, this should fail (connection refused / timeout).
	mux.HandleFunc("GET /probe", func(w http.ResponseWriter, r *http.Request) {
		workerURL := os.Getenv("WORKER_URL")
		if workerURL == "" {
			shared.JSON(w, http.StatusOK, shared.Response{
				Service: "api",
				Status:  "skipped",
				Port:    port,
				Message: "WORKER_URL not set — set it to the worker's internal address to test network isolation",
			})
			return
		}

		log.Printf("api: probing worker at %s", workerURL)
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(workerURL + "/healthz")
		if err != nil {
			log.Printf("api: probe FAILED (network isolation working): %v", err)
			shared.JSON(w, http.StatusOK, shared.Response{
				Service: "api",
				Status:  "isolated",
				Port:    port,
				Message: fmt.Sprintf("cannot reach worker at %s: %v — network isolation is working", workerURL, err),
			})
			return
		}
		defer resp.Body.Close()

		log.Printf("api: probe SUCCEEDED (network isolation BROKEN): status %d", resp.StatusCode)
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "api",
			Status:  "NOT_ISOLATED",
			Port:    port,
			Message: fmt.Sprintf("reached worker at %s — got HTTP %d — network isolation is BROKEN", workerURL, resp.StatusCode),
		})
	})

	log.Printf("api: listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

package main

import (
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
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

	log.Printf("api: listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

package worker

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/unkeyed/mono-repo-test/pkg/shared"
)

func Run() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9090"
	}

	var ready atomic.Bool
	var forceFail atomic.Bool

	// Simulate startup: worker needs to "warm up" before it's ready
	log.Println("worker: warming up...")
	go func() {
		time.Sleep(2 * time.Second)
		ready.Store(true)
		log.Println("worker: warm-up complete, ready")
	}()

	// Handle shutdown signals — log which signal and do cleanup
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGKILL)
	go func() {
		s := <-sig
		log.Printf("worker: received %s", s)
		log.Println("worker: flushing pending work...")
		time.Sleep(2 * time.Second) // simulate flush
		log.Printf("worker: clean shutdown after %s", s)
		os.Exit(0)
	}()

	// Background work loop
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		batch := 0
		for range tick.C {
			batch++
			log.Printf("worker: processing batch %d...", batch)
		}
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			shared.JSON(w, http.StatusServiceUnavailable, shared.Response{
				Service: "worker",
				Status:  "not_ready",
				Port:    port,
				Message: "still warming up",
			})
			return
		}
		if forceFail.Load() {
			shared.JSON(w, http.StatusServiceUnavailable, shared.Response{
				Service: "worker",
				Status:  "unhealthy",
				Port:    port,
				Message: "health manually toggled to fail",
			})
			return
		}
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "worker",
			Status:  "healthy",
			Port:    port,
		})
	})

	mux.HandleFunc("POST /healthz/fail", func(w http.ResponseWriter, r *http.Request) {
		forceFail.Store(true)
		log.Println("worker: healthcheck toggled to FAIL")
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "worker",
			Status:  "ok",
			Port:    port,
			Message: "healthcheck will now fail",
		})
	})

	mux.HandleFunc("POST /healthz/recover", func(w http.ResponseWriter, r *http.Request) {
		forceFail.Store(false)
		log.Println("worker: healthcheck toggled to PASS")
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "worker",
			Status:  "ok",
			Port:    port,
			Message: "healthcheck will now pass",
		})
	})

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "worker",
			Status:  "ok",
			Port:    port,
			Message: "background worker running",
		})
	})

	// GET /probe — try to reach the API service to test network isolation.
	// Set API_URL env var to the api's internal address.
	// If apps are properly isolated, this should fail (connection refused / timeout).
	mux.HandleFunc("GET /probe", func(w http.ResponseWriter, r *http.Request) {
		apiURL := os.Getenv("API_URL")
		if apiURL == "" {
			shared.JSON(w, http.StatusOK, shared.Response{
				Service: "worker",
				Status:  "skipped",
				Port:    port,
				Message: "API_URL not set — set it to the api's internal address to test network isolation",
			})
			return
		}

		log.Printf("worker: probing api at %s", apiURL)
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(apiURL + "/healthz")
		if err != nil {
			log.Printf("worker: probe FAILED (network isolation working): %v", err)
			shared.JSON(w, http.StatusOK, shared.Response{
				Service: "worker",
				Status:  "isolated",
				Port:    port,
				Message: fmt.Sprintf("cannot reach api at %s: %v — network isolation is working", apiURL, err),
			})
			return
		}
		defer resp.Body.Close()

		log.Printf("worker: probe SUCCEEDED (network isolation BROKEN): status %d", resp.StatusCode)
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: "worker",
			Status:  "NOT_ISOLATED",
			Port:    port,
			Message: fmt.Sprintf("reached api at %s — got HTTP %d — network isolation is BROKEN", apiURL, resp.StatusCode),
		})
	})

	log.Printf("worker: listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

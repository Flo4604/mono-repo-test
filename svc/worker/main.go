package main

import (
	"log"
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

	log.Printf("worker: listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

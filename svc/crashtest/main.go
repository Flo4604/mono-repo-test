// crashtest is a deliberately-unstable HTTP service for exercising the
// container lifecycle event pipeline: kubelet's ContainerStatus.Terminated
// captures, OOMKilled detection, and CrashLoopBackOff handling.
//
// It runs as an ordinary HTTP server when healthy. Each endpoint triggers
// a different failure mode so you can drive the matching ClickHouse row
// in instance_events_raw_v1.
//
// Failure-mode endpoints (POST):
//
//	/exit?code=N          os.Exit(code) — produces event_kind=terminated,
//	                      exit_code=N, reason="Error" (or "Completed" if
//	                      code=0; kubelet treats exit=0 as success).
//	/panic?msg=...        Go panic — exit code 2, reason="Error".
//	/sigsegv              nil pointer deref — Go runtime fatal, exit 2.
//	/sigkill              kill -9 self — exit code 137 with no OOM context.
//	                      Useful for distinguishing "kernel killed" from
//	                      "kernel killed because OOM"; kubelet should NOT
//	                      set Reason: OOMKilled for this case.
//	/sigterm              kill -15 self — graceful shutdown.
//	/oom?mb=N             allocate N MiB and write a byte to every 4 KiB
//	                      page so RSS actually commits. When N exceeds the
//	                      container memory limit kernel OOMKills the pod;
//	                      kubelet records exit=137 + Reason="OOMKilled".
//	                      hold=true (default) keeps the allocation forever;
//	                      hold=false releases after writing (weaker).
//	/oom-step?mb_per_sec=N&total_mb=M    gradual leak — useful when you
//	                      don't know the exact limit. Allocates in 1s
//	                      increments until total_mb reached or kernel
//	                      kills. Reason: OOMKilled when the limit hits.
//	/break-healthz        flip /healthz to 503. Combined with a liveness
//	                      probe, kubelet restarts the pod after enough
//	                      consecutive failures (default 3 with 10s period).
//	/fix-healthz          restore /healthz to 200.
//	/slow-healthz?ms=N    make /healthz take N ms; tripping the probe
//	                      timeout (default 1s) marks the pod unready.
//	/hang                 block forever. Tests termination-grace handling.
//	/hot-loop             100% CPU busy-loop. Tests CPU throttling +
//	                      noisy-neighbour scenarios.
//
// Healthy endpoints (GET):
//
//	/                     index — JSON list of every endpoint.
//	/healthz              200 OK by default; 503 after /break-healthz.
//	/info                 pid, hostname, uptime, cgroup memory limit (if
//	                      readable), goroutine + heap stats. Useful to
//	                      sanity-check that the limit you think you set is
//	                      what the kernel actually applied.
//
// Env-var crash modes (read at startup; combine to trigger CrashLoopBackOff
// without ever hitting an endpoint):
//
//	CRASH_ON_START=1                 exit before serving.
//	CRASH_ON_START_AFTER=10s         wait this long, then crash.
//	CRASH_EXIT_CODE=137              exit code for CRASH_ON_START
//	                                 (default 1).
//	OOM_ON_START_MB=512              allocate this much immediately at
//	                                 startup, before serving. Triggers OOM
//	                                 if it exceeds the container limit.
//	PANIC_ON_START=1                 panic before serving.
//
// All sizes are MiB. Default port 8080. Override with PORT.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

const serviceName = "crashtest"
const pageSize = 4096

var startedAt = time.Now()

func main() {
	// Env-driven failure modes execute before we bind a port so kubelet
	// sees the failure as a startup crash. This is what triggers
	// CrashLoopBackOff: kubelet retries the container, our env vars are
	// still set, and the pod fails again.
	runStartupCrashHooks()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	state := newState()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, indexResponse())
	})
	mux.HandleFunc("/healthz", state.handleHealthz)
	mux.HandleFunc("/info", state.handleInfo)
	mux.HandleFunc("/exit", handleExit)
	mux.HandleFunc("/panic", handlePanic)
	mux.HandleFunc("/sigsegv", handleSigsegv)
	mux.HandleFunc("/sigkill", handleSignal(syscall.SIGKILL))
	mux.HandleFunc("/sigterm", handleSignal(syscall.SIGTERM))
	mux.HandleFunc("/oom", handleOOM)
	mux.HandleFunc("/oom-step", handleOOMStep)
	mux.HandleFunc("/break-healthz", state.handleBreakHealthz)
	mux.HandleFunc("/fix-healthz", state.handleFixHealthz)
	mux.HandleFunc("/slow-healthz", state.handleSlowHealthz)
	mux.HandleFunc("/hang", handleHang)
	mux.HandleFunc("/hot-loop", handleHotLoop)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Trap SIGTERM/SIGINT so /sigterm and `kubectl delete pod` shut down
	// gracefully. SIGKILL bypasses this — the kernel won't deliver
	// SIGKILL to a Go handler — so /sigkill still produces exit 137.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		log.Printf("%s listening on :%s", serviceName, port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutdown signal received, draining…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// runStartupCrashHooks honours the CRASH_ON_START / OOM_ON_START_MB /
// PANIC_ON_START env vars. Returns only when no startup crash is
// configured; otherwise the process never returns normally from here.
func runStartupCrashHooks() {
	if delay := os.Getenv("CRASH_ON_START_AFTER"); delay != "" {
		if d, err := time.ParseDuration(delay); err == nil && d > 0 {
			log.Printf("CRASH_ON_START_AFTER=%s — sleeping then exiting", d)
			time.Sleep(d)
			os.Exit(envExitCode())
		}
	}
	if os.Getenv("CRASH_ON_START") == "1" {
		log.Printf("CRASH_ON_START=1 — exiting immediately with code %d", envExitCode())
		os.Exit(envExitCode())
	}
	if os.Getenv("PANIC_ON_START") == "1" {
		panic("PANIC_ON_START=1")
	}
	if mbStr := os.Getenv("OOM_ON_START_MB"); mbStr != "" {
		if mb, err := strconv.Atoi(mbStr); err == nil && mb > 0 {
			log.Printf("OOM_ON_START_MB=%d — allocating before serving", mb)
			allocateAndCommit(mb)
			// If we reach here the limit didn't trip; warn so the operator
			// notices their memory cap is too high to test OOMKilled.
			log.Printf("OOM_ON_START_MB=%d allocation succeeded — limit too high to trigger kill", mb)
		}
	}
}

func envExitCode() int {
	if v := os.Getenv("CRASH_EXIT_CODE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 1
}

// state holds the toggles that endpoints flip at runtime. Atomics keep the
// healthz handler lock-free so the kubelet probe never queues behind a
// /break-healthz request.
type state struct {
	healthzBroken atomic.Bool
	healthzDelay  atomic.Int64 // milliseconds
}

func newState() *state {
	return &state{}
}

func (s *state) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if d := s.healthzDelay.Load(); d > 0 {
		time.Sleep(time.Duration(d) * time.Millisecond)
	}
	if s.healthzBroken.Load() {
		http.Error(w, "broken by /break-healthz", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *state) handleBreakHealthz(w http.ResponseWriter, _ *http.Request) {
	s.healthzBroken.Store(true)
	writeJSON(w, http.StatusOK, map[string]any{"healthz": "broken"})
}

func (s *state) handleFixHealthz(w http.ResponseWriter, _ *http.Request) {
	s.healthzBroken.Store(false)
	s.healthzDelay.Store(0)
	writeJSON(w, http.StatusOK, map[string]any{"healthz": "ok"})
}

func (s *state) handleSlowHealthz(w http.ResponseWriter, r *http.Request) {
	ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
	if ms < 0 {
		ms = 0
	}
	s.healthzDelay.Store(int64(ms))
	writeJSON(w, http.StatusOK, map[string]any{"healthz_delay_ms": ms})
}

func (s *state) handleInfo(w http.ResponseWriter, _ *http.Request) {
	hostname, _ := os.Hostname()
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	writeJSON(w, http.StatusOK, map[string]any{
		"service":           serviceName,
		"pid":               os.Getpid(),
		"ppid":              os.Getppid(),
		"hostname":          hostname,
		"started_at":        startedAt.UTC().Format(time.RFC3339),
		"uptime_seconds":    int(time.Since(startedAt).Seconds()),
		"goroutines":        runtime.NumGoroutine(),
		"heap_alloc_bytes":  memStats.HeapAlloc,
		"heap_sys_bytes":    memStats.HeapSys,
		"sys_bytes":         memStats.Sys,
		"cgroup_memory_max": readCgroupMemoryMax(),
		"go_max_procs":      runtime.GOMAXPROCS(0),
		"healthz_broken":    s.healthzBroken.Load(),
		"healthz_delay_ms":  s.healthzDelay.Load(),
	})
}

// readCgroupMemoryMax tries cgroup v2 first (memory.max under
// /sys/fs/cgroup) then v1 (memory.limit_in_bytes). Returns "unknown" if
// neither is readable. "max" in v2 means no limit.
func readCgroupMemoryMax() string {
	for _, path := range []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return string(b)
	}
	return "unknown"
}

func handleExit(w http.ResponseWriter, r *http.Request) {
	code, _ := strconv.Atoi(r.URL.Query().Get("code"))
	writeJSON(w, http.StatusOK, map[string]any{"exiting_with": code})
	flushAndDelay(w)
	log.Printf("exiting with code %d on request", code)
	os.Exit(code)
}

func handlePanic(w http.ResponseWriter, r *http.Request) {
	msg := r.URL.Query().Get("msg")
	if msg == "" {
		msg = "test panic"
	}
	writeJSON(w, http.StatusOK, map[string]any{"panicking_with": msg})
	flushAndDelay(w)
	// Disable the default crash-dump suppression so kubelet captures the
	// trace in last-terminated message — useful when validating that
	// `pod.Status.ContainerStatuses[].LastTerminationState.Terminated.Message`
	// makes it through to ClickHouse.
	debug.SetTraceback("all")
	// Panic in a fresh goroutine, NOT inside the request handler. The
	// net/http server installs a per-handler recover() that swallows
	// panics so the rest of the server keeps running — exactly the
	// opposite of what we want here. An unrecovered goroutine panic
	// terminates the entire program with exit code 2.
	go func() {
		panic(msg)
	}()
	// Park the caller so the kernel can flush the response before the
	// runtime-error stack trace floods stderr.
	time.Sleep(2 * time.Second)
}

func handleSigsegv(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"action": "nil-pointer-deref"})
	flushAndDelay(w)
	go func() {
		var p *int
		_ = *p
	}()
	// Keep the response goroutine alive long enough for the segfault to
	// land; Go reports the runtime error and exits 2.
	time.Sleep(2 * time.Second)
}

func handleSignal(sig syscall.Signal) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sending_signal": sig.String()})
		flushAndDelay(w)
		log.Printf("sending %s to self", sig)
		_ = syscall.Kill(os.Getpid(), sig)
	}
}

func handleOOM(w http.ResponseWriter, r *http.Request) {
	mb, _ := strconv.Atoi(r.URL.Query().Get("mb"))
	if mb <= 0 {
		mb = 1024
	}
	hold := r.URL.Query().Get("hold") != "false"
	writeJSON(w, http.StatusOK, map[string]any{"allocating_mb": mb, "hold": hold})
	flushAndDelay(w)
	go func() {
		buf := allocateAndCommit(mb)
		if !hold {
			runtime.KeepAlive(buf)
			return
		}
		// Block forever holding the allocation. The kernel OOM killer
		// fires when this exceeds the container memory limit; the
		// container exits with code 137 and Reason: OOMKilled.
		select {}
	}()
}

func handleOOMStep(w http.ResponseWriter, r *http.Request) {
	mbPerSec, _ := strconv.Atoi(r.URL.Query().Get("mb_per_sec"))
	totalMB, _ := strconv.Atoi(r.URL.Query().Get("total_mb"))
	if mbPerSec <= 0 {
		mbPerSec = 64
	}
	if totalMB <= 0 {
		totalMB = 4096
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mb_per_sec": mbPerSec,
		"total_mb":   totalMB,
	})
	flushAndDelay(w)
	go func() {
		// Hold each tick's allocation alive in a slice-of-slices so the GC
		// can't reclaim it. Forces a steady RSS climb until the kernel
		// kills us or we hit total_mb.
		held := make([][]byte, 0, (totalMB/mbPerSec)+1)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		allocated := 0
		for range ticker.C {
			step := mbPerSec
			if allocated+step > totalMB {
				step = totalMB - allocated
			}
			if step <= 0 {
				return
			}
			held = append(held, allocateAndCommit(step))
			allocated += step
			log.Printf("oom-step: allocated %d MiB total", allocated)
		}
	}()
}

// allocateAndCommit allocates mb mebibytes and writes a byte to every 4
// KiB page. The page write is what forces the kernel to actually commit
// physical memory — without it Go's runtime hands out a virtual mapping
// that never trips the OOM killer until first touch.
func allocateAndCommit(mb int) []byte {
	bytes := mb * 1024 * 1024
	buf := make([]byte, bytes)
	for i := 0; i < bytes; i += pageSize {
		buf[i] = 0xff
	}
	return buf
}

func handleHang(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"action": "hanging-forever"})
	flushAndDelay(w)
	select {}
}

func handleHotLoop(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"action": "busy-loop"})
	flushAndDelay(w)
	go func() {
		// Pin one core. CPU-throttled environments will delay scheduling;
		// CFS-throttled containers will see this saturate their quota.
		for {
			_ = 1
		}
	}()
}

// flushAndDelay forces the response buffer out to the kernel and gives
// the kernel a moment to flush to the client before we crash. Without
// this, callers driving the test get a connection reset instead of the
// JSON acknowledging which mode was triggered.
func flushAndDelay(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	time.Sleep(200 * time.Millisecond)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func indexResponse() map[string]any {
	return map[string]any{
		"service": serviceName,
		"endpoints": map[string]string{
			"GET  /":                    "this listing",
			"GET  /healthz":             "200 OK; 503 after /break-healthz",
			"GET  /info":                "pid, uptime, cgroup memory limit, runtime stats",
			"POST /exit?code=N":         "os.Exit(code) — default 1",
			"POST /panic?msg=...":       "Go panic — exit 2",
			"POST /sigsegv":             "nil pointer deref — exit 2",
			"POST /sigkill":             "kill -9 self — exit 137 (no OOM context)",
			"POST /sigterm":             "kill -15 self — graceful shutdown",
			"POST /oom?mb=N&hold=true":  "allocate N MiB and commit; trips OOMKilled when N > limit",
			"POST /oom-step":            "gradual leak — mb_per_sec & total_mb",
			"POST /break-healthz":       "make /healthz return 503",
			"POST /fix-healthz":         "restore /healthz to 200 OK",
			"POST /slow-healthz?ms=N":   "delay /healthz by N ms",
			"POST /hang":                "block forever — tests grace period",
			"POST /hot-loop":            "busy-loop — tests CPU throttling",
		},
		"env_crash_modes": map[string]string{
			"CRASH_ON_START":       "set to 1 to exit before serving (CrashLoopBackOff)",
			"CRASH_ON_START_AFTER": "duration like 10s — delayed crash",
			"CRASH_EXIT_CODE":      "exit code for CRASH_ON_START (default 1)",
			"OOM_ON_START_MB":      "allocate this much MiB at startup",
			"PANIC_ON_START":       "set to 1 to panic before serving",
		},
		"started_at": startedAt.UTC().Format(time.RFC3339),
	}
}


// netio is a combined test service for exercising heimdall's network and
// disk metering. It's deployed as a single pod that serves both /net/*
// and /disk/* so you can drive network + disk load from the same place.
//
// Endpoints:
//   GET  /                     info
//   GET  /healthz              readiness probe
//   POST /healthz              same
//
//   GET  /net/info             local hostname, env, dest defaults
//   GET  /net/public?mb=N      download N MiB from a public URL         (pod INGRESS public)
//   GET  /net/upload?mb=N      POST N MiB of random bytes to a public   (pod EGRESS public)
//                              endpoint (defaults to cloudflare __up)
//   GET  /net/private?mb=N     call an in-cluster service until N MiB   (pod EGRESS+INGRESS private)
//                              drained
//   GET  /net/sink?mb=N        server streams N MiB of zeros to caller  (pod EGRESS to caller)
//
//   GET  /disk/info            statfs on the scratch dir
//   GET  /disk/write?size_mb=N write+read verify on scratch dir
//   GET  /disk/fill?percent=N  fill scratch dir to target %
//   GET  /disk/clean           remove all files from scratch dir
//
// All sizes are in MiB. Network bytes go to /dev/null on either side, so
// it's purely a network exercise; each handler reports actual transfer
// size + elapsed so you can sanity-check heimdall against what the service
// actually moved.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/unkeyed/mono-repo-test/pkg/shared"
)

const serviceName = "netio"

// Hetzner Ashburn speedtest endpoint. The 1GB.bin file supports Range so
// we can grab any number of bytes from 1 to 1 GiB in one fetch.
const (
	publicURL    = "https://ash-speed.hetzner.com/1GB.bin"
	publicMaxMiB = 1024
)

// Cloudflare speed test upload endpoint. Accepts POST of arbitrary size
// and returns a tiny ack, so the dominant traffic is the request body
// going OUT of the pod — which is what we want for egress testing.
const uploadURL = "https://speed.cloudflare.com/__up"

// Default in-cluster destination when /net/private is called without a host.
const (
	defaultPrivateHost = "krane.unkey.svc.cluster.local:8070"
	defaultPrivatePath = "/health"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3459"
	}
	scratchDir := os.Getenv("UNKEY_EPHEMERAL_DISK_PATH")
	if scratchDir == "" {
		scratchDir = "/data"
	}

	var ready atomic.Bool
	startupDelay := 2 * time.Second
	log.Printf("%s: starting up, will be ready in %s", serviceName, startupDelay)
	go func() {
		time.Sleep(startupDelay)
		ready.Store(true)
		log.Printf("%s: ready to serve traffic", serviceName)
	}()

	var inflight atomic.Int64

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	go func() {
		s := <-sig
		log.Printf("%s: received %s — starting graceful shutdown", serviceName, s)
		deadline := time.After(10 * time.Second)
		for inflight.Load() > 0 {
			select {
			case <-deadline:
				log.Printf("%s: shutdown deadline hit with %d in-flight", serviceName, inflight.Load())
				os.Exit(1)
			default:
				time.Sleep(100 * time.Millisecond)
			}
		}
		log.Printf("%s: clean shutdown after %s", serviceName, s)
		os.Exit(0)
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: serviceName,
			Status:  "ok",
			Port:    port,
			Message: "endpoints: /net/{info,public,upload,private,sink} /disk/{info,write,fill,clean}",
		})
	})

	health := func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			shared.JSON(w, http.StatusServiceUnavailable, shared.Response{
				Service: serviceName, Status: "not_ready", Port: port,
			})
			return
		}
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: serviceName, Status: "healthy", Port: port,
		})
	}
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("POST /healthz", health)

	registerNetRoutes(mux, &inflight, port)
	registerDiskRoutes(mux, &inflight, port, scratchDir)

	log.Printf("%s: listening on :%s (scratch dir: %s)", serviceName, port, scratchDir)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func registerNetRoutes(mux *http.ServeMux, inflight *atomic.Int64, port string) {
	// /net/info — what the service knows about its own network identity.
	mux.HandleFunc("GET /net/info", func(w http.ResponseWriter, r *http.Request) {
		host, _ := os.Hostname()
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: serviceName,
			Status:  "ok",
			Port:    port,
			Message: fmt.Sprintf("host=%s instance=%s region=%s default_public=%s default_upload=%s default_private=http://%s%s",
				host,
				os.Getenv("UNKEY_INSTANCE_ID"),
				os.Getenv("UNKEY_REGION"),
				publicURL, uploadURL,
				defaultPrivateHost, defaultPrivatePath,
			),
		})
	})

	// /net/public — pod pulls N MiB from a public URL (drives INGRESS).
	mux.HandleFunc("GET /net/public", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		mb := parseMB(r, 5, publicMaxMiB)
		url := r.URL.Query().Get("url")
		useRange := url == ""
		if url == "" {
			url = publicURL
		}

		log.Printf("%s: GET %s mb=%d range=%v", serviceName, url, mb, useRange)
		start := time.Now()
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
		if useRange {
			req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", int64(mb)*1024*1024-1))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			shared.JSON(w, http.StatusBadGateway, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("public GET failed: %v", err),
			})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			shared.JSON(w, http.StatusBadGateway, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("upstream returned %d (cl=%s) body=%q",
					resp.StatusCode, resp.Header.Get("Content-Length"), string(body)),
			})
			return
		}
		n, err := io.Copy(io.Discard, resp.Body)
		dur := time.Since(start)
		if err != nil {
			shared.JSON(w, http.StatusOK, shared.Response{
				Service: serviceName, Status: "partial", Port: port,
				Message: fmt.Sprintf("read %d bytes in %s before error: %v", n, dur, err),
			})
			return
		}
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: serviceName, Status: "ok", Port: port,
			Message: fmt.Sprintf("downloaded %d bytes (%d MiB) from %s in %s (%.1f MiB/s)",
				n, n/1024/1024, url, dur.Round(time.Millisecond),
				float64(n)/(1024*1024)/dur.Seconds()),
		})
	})

	// /net/upload — pod POSTs N MiB to a public endpoint (drives EGRESS).
	// Dominant traffic is the request body going OUT; the ack response is
	// tiny. Default endpoint is cloudflare's __up which accepts arbitrary
	// size uploads.
	mux.HandleFunc("GET /net/upload", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		mb := parseMB(r, 5, publicMaxMiB)
		url := r.URL.Query().Get("url")
		if url == "" {
			url = uploadURL
		}
		size := int64(mb) * 1024 * 1024

		log.Printf("%s: POST %s mb=%d", serviceName, url, mb)
		start := time.Now()
		// io.LimitReader(rand.Reader, size) streams random bytes so the
		// full payload is generated on-the-fly rather than buffered; this
		// exercises the egress path without allocating N MiB up front.
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, io.LimitReader(rand.Reader, size))
		if err != nil {
			shared.JSON(w, http.StatusInternalServerError, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("build request failed: %v", err),
			})
			return
		}
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			shared.JSON(w, http.StatusBadGateway, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("public POST failed: %v", err),
			})
			return
		}
		defer resp.Body.Close()
		ackN, _ := io.Copy(io.Discard, resp.Body)
		dur := time.Since(start)

		if resp.StatusCode >= 400 {
			shared.JSON(w, http.StatusBadGateway, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("upstream returned %d after %d MiB in %s",
					resp.StatusCode, mb, dur.Round(time.Millisecond)),
			})
			return
		}
		shared.JSON(w, http.StatusOK, shared.Response{
			Service: serviceName, Status: "ok", Port: port,
			Message: fmt.Sprintf("uploaded %d bytes (%d MiB) to %s in %s (%.1f MiB/s) ack=%dB status=%d",
				size, mb, url, dur.Round(time.Millisecond),
				float64(size)/(1024*1024)/dur.Seconds(), ackN, resp.StatusCode),
		})
	})

	// /net/private — drive private egress via in-cluster loop.
	mux.HandleFunc("GET /net/private", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		mb := parseMB(r, 1, publicMaxMiB)
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

		log.Printf("%s: looping GET %s until %d bytes drained", serviceName, url, budget)
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
					Service: serviceName, Status: "error", Port: port,
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
			Service: serviceName, Status: "ok", Port: port,
			Message: fmt.Sprintf("drained %d bytes via %d calls to %s in %s (%.1f KiB/s)",
				total, calls, url, dur.Round(time.Millisecond), float64(total)/1024/dur.Seconds()),
		})
	})

	// /net/sink — server emits N MiB of zeros to the caller.
	mux.HandleFunc("GET /net/sink", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		mb := parseMB(r, 5, publicMaxMiB)
		size := int64(mb) * 1024 * 1024

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)

		start := time.Now()
		n, err := io.CopyN(w, rand.Reader, size)
		dur := time.Since(start)
		log.Printf("%s: sink served %d bytes in %s (err=%v)", serviceName, n, dur, err)
	})
}

func registerDiskRoutes(mux *http.ServeMux, inflight *atomic.Int64, port, scratchDir string) {
	mux.HandleFunc("GET /disk/info", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)

		var stat syscall.Statfs_t
		if err := syscall.Statfs(scratchDir, &stat); err != nil {
			shared.JSON(w, http.StatusInternalServerError, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("statfs %s failed: %v", scratchDir, err),
			})
			return
		}

		totalBytes := stat.Blocks * uint64(stat.Bsize)
		freeBytes := stat.Bfree * uint64(stat.Bsize)
		usedBytes := totalBytes - freeBytes

		shared.JSON(w, http.StatusOK, shared.Response{
			Service: serviceName, Status: "ok", Port: port,
			Message: fmt.Sprintf("path=%s total=%dMiB used=%dMiB free=%dMiB",
				scratchDir, totalBytes/1024/1024, usedBytes/1024/1024, freeBytes/1024/1024),
		})
	})

	// /disk/write — write random bytes, read them back, verify checksum.
	mux.HandleFunc("GET /disk/write", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)

		sizeMB := 10
		if s := r.URL.Query().Get("size_mb"); s != "" {
			if parsed, err := strconv.Atoi(s); err == nil && parsed > 0 && parsed <= 1024 {
				sizeMB = parsed
			}
		}

		filename := filepath.Join(scratchDir, fmt.Sprintf("test-%d.bin", time.Now().UnixNano()))
		sizeBytes := int64(sizeMB) * 1024 * 1024

		log.Printf("%s: writing %d MiB to %s", serviceName, sizeMB, filename)
		start := time.Now()

		f, err := os.Create(filename)
		if err != nil {
			shared.JSON(w, http.StatusInternalServerError, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("create failed: %v", err),
			})
			return
		}

		writeHash := sha256.New()
		writer := io.MultiWriter(f, writeHash)
		if _, err := io.CopyN(writer, rand.Reader, sizeBytes); err != nil {
			f.Close()
			os.Remove(filename)
			shared.JSON(w, http.StatusInternalServerError, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("write failed: %v", err),
			})
			return
		}
		f.Close()
		writeDuration := time.Since(start)
		writeChecksum := hex.EncodeToString(writeHash.Sum(nil))

		readStart := time.Now()
		f2, err := os.Open(filename)
		if err != nil {
			os.Remove(filename)
			shared.JSON(w, http.StatusInternalServerError, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("open for read failed: %v", err),
			})
			return
		}
		readHash := sha256.New()
		if _, err := io.Copy(readHash, f2); err != nil {
			f2.Close()
			os.Remove(filename)
			shared.JSON(w, http.StatusInternalServerError, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("read failed: %v", err),
			})
			return
		}
		f2.Close()
		readDuration := time.Since(readStart)
		readChecksum := hex.EncodeToString(readHash.Sum(nil))

		os.Remove(filename)

		verified := writeChecksum == readChecksum
		status := "ok"
		if !verified {
			status = "checksum_mismatch"
		}

		writeMBps := float64(sizeMB) / writeDuration.Seconds()
		readMBps := float64(sizeMB) / readDuration.Seconds()

		log.Printf("%s: wrote %d MiB in %s (%.1f MB/s), read in %s (%.1f MB/s), verified=%v",
			serviceName, sizeMB, writeDuration, writeMBps, readDuration, readMBps, verified)

		shared.JSON(w, http.StatusOK, shared.Response{
			Service: serviceName, Status: status, Port: port,
			Message: fmt.Sprintf(
				"size=%dMiB write=%s(%.1fMB/s) read=%s(%.1fMB/s) verified=%v checksum=%s",
				sizeMB, writeDuration.Round(time.Millisecond), writeMBps,
				readDuration.Round(time.Millisecond), readMBps, verified,
				writeChecksum[:16],
			),
		})
	})

	// /disk/fill — grow scratch dir toward a target % of filesystem usage.
	mux.HandleFunc("GET /disk/fill", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)

		targetPercent := 80
		if s := r.URL.Query().Get("percent"); s != "" {
			if parsed, err := strconv.Atoi(s); err == nil && parsed > 0 && parsed <= 99 {
				targetPercent = parsed
			}
		}

		var stat syscall.Statfs_t
		if err := syscall.Statfs(scratchDir, &stat); err != nil {
			shared.JSON(w, http.StatusInternalServerError, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("statfs failed: %v", err),
			})
			return
		}

		totalBytes := stat.Blocks * uint64(stat.Bsize)
		freeBytes := stat.Bfree * uint64(stat.Bsize)
		usedBytes := totalBytes - freeBytes
		currentPercent := int(usedBytes * 100 / totalBytes)

		if currentPercent >= targetPercent {
			shared.JSON(w, http.StatusOK, shared.Response{
				Service: serviceName, Status: "ok", Port: port,
				Message: fmt.Sprintf("already at %d%% (target %d%%)", currentPercent, targetPercent),
			})
			return
		}

		targetUsed := totalBytes * uint64(targetPercent) / 100
		bytesToWrite := int64(targetUsed - usedBytes)

		log.Printf("%s: filling disk from %d%% to %d%% (%d MiB to write)",
			serviceName, currentPercent, targetPercent, bytesToWrite/1024/1024)

		filename := filepath.Join(scratchDir, fmt.Sprintf("fill-%d.bin", time.Now().UnixNano()))
		f, err := os.Create(filename)
		if err != nil {
			shared.JSON(w, http.StatusInternalServerError, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("create fill file failed: %v", err),
			})
			return
		}

		start := time.Now()
		written, err := io.CopyN(f, rand.Reader, bytesToWrite)
		f.Close()
		duration := time.Since(start)

		if err != nil {
			shared.JSON(w, http.StatusOK, shared.Response{
				Service: serviceName, Status: "partial", Port: port,
				Message: fmt.Sprintf("wrote %d MiB before error: %v (disk may be full)", written/1024/1024, err),
			})
			return
		}

		shared.JSON(w, http.StatusOK, shared.Response{
			Service: serviceName, Status: "ok", Port: port,
			Message: fmt.Sprintf("filled disk to ~%d%% — wrote %d MiB in %s",
				targetPercent, written/1024/1024, duration.Round(time.Millisecond)),
		})
	})

	// /disk/clean — remove all files from scratch dir.
	mux.HandleFunc("GET /disk/clean", func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)

		entries, err := os.ReadDir(scratchDir)
		if err != nil {
			shared.JSON(w, http.StatusInternalServerError, shared.Response{
				Service: serviceName, Status: "error", Port: port,
				Message: fmt.Sprintf("readdir failed: %v", err),
			})
			return
		}

		removed := 0
		for _, entry := range entries {
			if err := os.Remove(filepath.Join(scratchDir, entry.Name())); err == nil {
				removed++
			}
		}

		shared.JSON(w, http.StatusOK, shared.Response{
			Service: serviceName, Status: "ok", Port: port,
			Message: fmt.Sprintf("removed %d files from %s", removed, scratchDir),
		})
	})
}

// parseMB reads ?mb=N. Defaults to `def` if missing/invalid; clamped to max.
func parseMB(r *http.Request, def, max int) int {
	s := r.URL.Query().Get("mb")
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		n = max
	}
	return n
}

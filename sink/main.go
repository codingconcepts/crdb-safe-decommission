package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type sink struct {
	mu          sync.Mutex
	rows        int
	lastPrinted int
}

func main() {
	log.SetFlags(0)

	addr := env("ADDR", ":8081")

	interval, err := time.ParseDuration(env("PRINT_INTERVAL", "1s"))
	if err != nil {
		log.Fatalf("invalid PRINT_INTERVAL: %v", err)
	}

	s := &sink{}

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /", s.put)

	go s.report(interval)

	log.Printf("sink listening on %s, reporting messages every %s", addr, interval)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("error serving: %v", err)
	}
}

// put is the handler that'll be called by the changefeed.
func (s *sink) put(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("error reading %s: %v", r.URL.Path, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	defer r.Body.Close()

	s.mu.Lock()
	s.rows += countLines(body)
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

// report logs the number of messages received since the last tick.
func (s *sink) report(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.Lock()
		delta := s.rows - s.lastPrinted
		s.lastPrinted = s.rows
		total := s.rows
		s.mu.Unlock()

		log.Printf("%d messages (%d total)", delta, total)
	}
}

func countLines(b []byte) int {
	var n int

	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}

	return n
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

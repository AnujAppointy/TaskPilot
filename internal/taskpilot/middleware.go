package taskpilot

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type serverMetrics struct {
	Requests uint64 `json:"requests"`
	Errors   uint64 `json:"errors"`
	Started  string `json:"started_at"`
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *responseRecorder) Flush() {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)
		applySecurityHeaders(w)
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		rec := &responseRecorder{ResponseWriter: w}
		atomic.AddUint64(&s.metrics.Requests, 1)
		next.ServeHTTP(rec, r)
		if rec.status >= 500 {
			atomic.AddUint64(&s.metrics.Errors, 1)
		}
		log.Printf("request_id=%s method=%s path=%s status=%d duration_ms=%d", requestID, r.Method, r.URL.Path, rec.status, time.Since(start).Milliseconds())
	})
}

func applySecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'")
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func metricsText(m serverMetrics, counts StoreStats) string {
	lines := []string{
		"# HELP taskpilot_requests_total Total HTTP requests.",
		"# TYPE taskpilot_requests_total counter",
		fmt.Sprintf("taskpilot_requests_total %d", m.Requests),
		"# HELP taskpilot_errors_total Total HTTP 5xx responses.",
		"# TYPE taskpilot_errors_total counter",
		fmt.Sprintf("taskpilot_errors_total %d", m.Errors),
		fmt.Sprintf("taskpilot_tasks_total %d", counts.Tasks),
		fmt.Sprintf("taskpilot_active_locks_total %d", counts.ActiveLocks),
		fmt.Sprintf("taskpilot_handoffs_total %d", counts.Handoffs),
	}
	return strings.Join(lines, "\n") + "\n"
}

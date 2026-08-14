package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/AlexAli29/orm/ormhealth"
)

// The three health endpoints, which answer three different questions.
//
// Conflating them is the most common way to make a deployment worse rather than
// better, so they are separate handlers with separate rules:
//
//	/livez            is this process alive?          asks nothing else
//	/readyz           can it serve traffic?           one round trip
//	/admin/db-health  what is the state of things?    a full read-only report
//
// The distinction is operational, not stylistic. A liveness probe that queries
// PostgreSQL restarts every application pod when the database has a bad minute,
// which turns a database incident into an outage. A readiness probe that ran
// schema reconciliation would hammer the catalog every two seconds. And the
// deep report is worth having but is far too expensive — and far too detailed —
// to expose on the path a load balancer polls.

// Health serves the probes.
type Health struct {
	// db is what the readiness and deep checks query. It is nil in a process
	// that has no database, and the handlers say so rather than panicking.
	db ormhealth.Querier
	// deepOpts are the checks the deep report runs, decided at startup.
	deepOpts []ormhealth.DeepOption
	// readyTimeout bounds the readiness query, because a probe that can hang
	// is a probe that stops answering.
	readyTimeout time.Duration
}

// NewHealth builds the health handlers.
func NewHealth(db ormhealth.Querier, deepOpts ...ormhealth.DeepOption) *Health {
	return &Health{db: db, deepOpts: deepOpts, readyTimeout: 2 * time.Second}
}

// Routes registers the probes on a mux.
func (h *Health) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /livez", h.Live)
	mux.HandleFunc("GET /readyz", h.Ready)
	mux.HandleFunc("GET /admin/db-health", h.Deep)
}

// Live answers whether the process is running.
//
// It touches nothing. Not the database, not the pool, not a cache — if this
// handler runs at all, the answer is yes, and that is the entire question a
// liveness probe asks. Adding a database check here would mean a database
// outage restarting every replica, which loses the in-flight work and the warm
// connections at exactly the moment they are hardest to rebuild.
func (h *Health) Live(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"alive"}` + "\n"))
}

// Ready answers whether this instance can serve traffic.
//
// It runs ormhealth.Quick, which is one SELECT 1 and nothing else — no catalog
// read, no version lookup, no schema reconciliation. That is what makes it safe
// on the interval a load balancer polls.
//
// A database that does not answer means not ready, which takes this instance
// out of rotation without restarting it.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "down", "reason": "no database configured",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.readyTimeout)
	defer cancel()

	report := ormhealth.Quick(ctx, h.db)
	body := map[string]any{
		"status":     string(report.Status),
		"latency_ms": report.Latency.Milliseconds(),
	}
	if !report.OK() {
		// The reason is a short classification rather than the driver's
		// message, which can carry the DSN.
		body["reason"] = "database unreachable"
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// Deep serves the operational report.
//
// It is read-only: ormhealth.Deep asks the catalog questions and writes
// nothing, and there is no option here that would change that. It is on an
// /admin path because it is expensive and because it describes the deployment —
// server version, pool saturation, whether the schema has drifted from the
// code. That is information for an operator, not for the internet.
//
// A real deployment puts authentication in front of this. The example does not
// ship one because the choice is the deployment's, and a fake one would look
// like a recommendation.
func (h *Health) Deep(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "down", "reason": "no database configured",
		})
		return
	}
	// A deep report reads several catalogs, so it gets a longer budget than the
	// readiness probe and still gets one.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	report := ormhealth.Deep(ctx, h.db, h.deepOpts...)
	status := http.StatusOK
	if report.Status != ormhealth.StatusUp {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(report)
}

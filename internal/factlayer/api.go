package factlayer

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// RegisterHandlers mounts the L0 fact-layer HTTP endpoints onto mux.
//
//	GET /api/facts          — current state tree (all facts)
//	GET /api/changes        — recent ledger entries (?n=100 or ?from=unix&to=unix)
func RegisterHandlers(mux *http.ServeMux, r *Runner) {
	mux.HandleFunc("/api/facts", func(w http.ResponseWriter, req *http.Request) {
		tree := r.StateAt(time.Now())
		if tree == nil {
			http.Error(w, `{"error":"not ready"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"taken_at": tree.TakenAt.Unix(),
			"facts":    tree.All(),
		})
	})

	mux.HandleFunc("/api/changes", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		var changes []Change

		fromStr := q.Get("from")
		toStr := q.Get("to")
		if fromStr != "" || toStr != "" {
			var t1, t2 time.Time
			if fromStr != "" {
				if fu, err := strconv.ParseInt(fromStr, 10, 64); err == nil {
					t1 = time.Unix(fu, 0)
				}
			} else {
				t1 = time.Now().Add(-24 * time.Hour)
			}
			if toStr != "" {
				if tu, err := strconv.ParseInt(toStr, 10, 64); err == nil {
					t2 = time.Unix(tu, 0)
				}
			} else {
				t2 = time.Now()
			}
			changes = r.ChangesBetween(t1, t2)
		} else {
			n := 100
			if nStr := q.Get("n"); nStr != "" {
				if parsed, err := strconv.Atoi(nStr); err == nil && parsed > 0 && parsed <= 5000 {
					n = parsed
				}
			}
			changes = r.RecentChanges(n)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"count":   len(changes),
			"changes": changes,
		})
	})
}

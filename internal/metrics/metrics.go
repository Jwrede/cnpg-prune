// Package metrics exposes the results of retention runs in the Prometheus
// text format.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Jwrede/cnpg-prune/internal/prune"
)

// Registry holds the state of the last run and the running totals.
type Registry struct {
	mu               sync.Mutex
	runs, failures   int
	deletedBackups   int
	deletedSnapshots int
	lastSuccess      time.Time
	planned          map[[3]string]int // namespace, cluster, action
	orphans          int
}

func New() *Registry { return &Registry{planned: map[[3]string]int{}} }

// Record stores the outcome of one run. plan may be nil when the run failed
// before a plan existed.
func (r *Registry) Record(plan *prune.Plan, res prune.Result, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs++
	r.deletedBackups += res.Backups
	r.deletedSnapshots += res.Snapshots
	if err != nil || len(res.Errors) > 0 {
		r.failures++
	} else {
		r.lastSuccess = time.Now()
	}
	if plan == nil {
		return
	}
	r.planned = map[[3]string]int{}
	for _, c := range plan.Clusters {
		for _, b := range c.Backups {
			r.planned[[3]string{c.Namespace, c.Name, b.Action}]++
		}
	}
	r.orphans = len(plan.Orphans)
}

// Write prints all metrics.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(w, "# HELP cnpg_prune_runs_total Retention runs.")
	fmt.Fprintln(w, "# TYPE cnpg_prune_runs_total counter")
	fmt.Fprintf(w, "cnpg_prune_runs_total %d\n", r.runs)
	fmt.Fprintln(w, "# HELP cnpg_prune_failed_runs_total Runs that failed or could not delete everything.")
	fmt.Fprintln(w, "# TYPE cnpg_prune_failed_runs_total counter")
	fmt.Fprintf(w, "cnpg_prune_failed_runs_total %d\n", r.failures)
	fmt.Fprintln(w, "# HELP cnpg_prune_deleted_total Objects deleted.")
	fmt.Fprintln(w, "# TYPE cnpg_prune_deleted_total counter")
	fmt.Fprintf(w, "cnpg_prune_deleted_total{kind=\"Backup\"} %d\n", r.deletedBackups)
	fmt.Fprintf(w, "cnpg_prune_deleted_total{kind=\"VolumeSnapshot\"} %d\n", r.deletedSnapshots)
	fmt.Fprintln(w, "# HELP cnpg_prune_last_success_timestamp_seconds End of the last run without errors.")
	fmt.Fprintln(w, "# TYPE cnpg_prune_last_success_timestamp_seconds gauge")
	last := 0.0
	if !r.lastSuccess.IsZero() {
		last = float64(r.lastSuccess.Unix())
	}
	fmt.Fprintf(w, "cnpg_prune_last_success_timestamp_seconds %g\n", last)
	fmt.Fprintln(w, "# HELP cnpg_prune_backups Backups per cluster and decision in the last run.")
	fmt.Fprintln(w, "# TYPE cnpg_prune_backups gauge")
	keys := make([][3]string, 0, len(r.planned))
	for k := range r.planned {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	for _, k := range keys {
		fmt.Fprintf(w, "cnpg_prune_backups{namespace=%q,cluster=%q,action=%q} %d\n", k[0], k[1], k[2], r.planned[k])
	}
	fmt.Fprintln(w, "# HELP cnpg_prune_orphans Orphaned VolumeSnapshots and Backups found in the last run.")
	fmt.Fprintln(w, "# TYPE cnpg_prune_orphans gauge")
	fmt.Fprintf(w, "cnpg_prune_orphans %d\n", r.orphans)
}

// Handler serves /metrics and /healthz.
func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		r.Write(w)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	return mux
}

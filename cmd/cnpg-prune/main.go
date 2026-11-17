// Command cnpg-prune applies retention policies to CloudNativePG Backup
// resources and their VolumeSnapshots.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Jwrede/cnpg-prune/internal/metrics"
	"github.com/Jwrede/cnpg-prune/internal/prune"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

var version = "dev"

const usage = `cnpg-prune applies retention policies to CloudNativePG backups.

Usage:
  cnpg-prune plan  [flags]   show what would be deleted (changes nothing)
  cnpg-prune apply [flags]   delete expired Backups and their VolumeSnapshots
  cnpg-prune run   [flags]   apply every -interval and serve /metrics
  cnpg-prune version

Policy per Cluster: spec.backup.retentionPolicy, or the annotations
cnpg-prune/retention ("30d", "4w", "6m") and cnpg-prune/keep-last ("7").
cnpg-prune/skip: "true" leaves a Cluster alone.

Flags:
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	if cmd == "version" {
		fmt.Println(version)
		return
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage); fs.PrintDefaults() }
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig file (default: $KUBECONFIG, ~/.kube/config, then in-cluster)")
	kubecontext := fs.String("context", "", "kubeconfig context")
	namespace := fs.String("namespace", "", "only this namespace (default: all)")
	methods := fs.String("method", "", "comma-separated backup methods to prune: volumeSnapshot, barmanObjectStore, plugin (default: all)")
	nowFlag := fs.String("now", "", "evaluate the policy as of this RFC 3339 time instead of the current time")
	orphans := fs.Bool("orphans", false, "apply/run: also delete orphaned VolumeSnapshots and Backups")
	grace := fs.Duration("orphan-grace", time.Hour, "ignore orphans younger than this")
	asJSON := fs.Bool("json", false, "plan: print JSON")
	interval := fs.Duration("interval", time.Hour, "run: time between runs")
	listen := fs.String("listen", ":8080", "run: address for /metrics and /healthz")
	if cmd != "plan" && cmd != "apply" && cmd != "run" {
		fs.Usage()
		os.Exit(2)
	}
	_ = fs.Parse(os.Args[2:])

	opts := prune.Options{Namespace: *namespace, OrphanGrace: *grace}
	if *methods != "" {
		opts.Methods = map[string]bool{}
		for _, m := range strings.Split(*methods, ",") {
			m = strings.TrimSpace(m)
			if m != prune.MethodVolumeSnapshot && m != prune.MethodBarman && m != prune.MethodPlugin {
				log.Fatalf("unknown backup method %q", m)
			}
			opts.Methods[m] = true
		}
	}
	var fixedNow time.Time
	if *nowFlag != "" {
		t, err := time.Parse(time.RFC3339, *nowFlag)
		if err != nil {
			log.Fatalf("-now: %v", err)
		}
		fixedNow = t
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = *kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: *kubecontext}).ClientConfig()
	if err != nil {
		log.Fatalf("kubernetes config: %v", err)
	}
	cfg.UserAgent = "cnpg-prune/" + version
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	once := func() (*prune.Plan, prune.Result, error) {
		opts.Now = fixedNow
		if opts.Now.IsZero() {
			opts.Now = time.Now()
		}
		plan, err := prune.Build(ctx, client, opts)
		if err != nil {
			return nil, prune.Result{}, err
		}
		return plan, prune.Apply(ctx, client, plan, *orphans), nil
	}

	switch cmd {
	case "plan":
		opts.Now = fixedNow
		plan, err := prune.Build(ctx, client, opts)
		if err != nil {
			log.Fatal(err)
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(plan)
			return
		}
		plan.WriteText(os.Stdout)
	case "apply":
		plan, res, err := once()
		if err != nil {
			log.Fatal(err)
		}
		plan.WriteText(os.Stdout)
		report(res, *orphans)
		if len(res.Errors) > 0 {
			os.Exit(1)
		}
	case "run":
		reg := metrics.New()
		srv := &http.Server{Addr: *listen, Handler: reg.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("metrics server: %v", err)
			}
		}()
		for {
			plan, res, err := once()
			reg.Record(plan, res, err)
			if err != nil {
				log.Printf("run failed: %v", err)
			} else {
				n := plan.Count()
				log.Printf("kept %d, deleted %d backups and %d volume snapshots, %d orphans found",
					n.Kept, res.Backups, res.Snapshots, n.Orphans)
				for _, e := range res.Errors {
					log.Print(e)
				}
			}
			select {
			case <-ctx.Done():
				shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = srv.Shutdown(shutdown)
				cancel()
				return
			case <-time.After(*interval):
			}
		}
	}
}

func report(res prune.Result, orphans bool) {
	fmt.Printf("Deleted %d backups and %d volume snapshots", res.Backups, res.Snapshots)
	if !orphans {
		fmt.Print(" (orphans left alone, use -orphans)")
	}
	fmt.Println()
	for _, e := range res.Errors {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
}

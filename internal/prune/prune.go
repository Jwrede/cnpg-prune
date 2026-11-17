// Package prune builds and applies retention plans for CloudNativePG backups.
package prune

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jwrede/cnpg-prune/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	ClusterGVR  = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	BackupGVR   = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups"}
	SnapshotGVR = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
	ContentGVR  = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshotcontents"}
)

// Annotations on a Cluster that configure cnpg-prune.
const (
	AnnotationRetention = "cnpg-prune/retention"
	AnnotationKeepLast  = "cnpg-prune/keep-last"
	AnnotationSkip      = "cnpg-prune/skip"
)

// LabelBackupName is the label CloudNativePG puts on the VolumeSnapshots of a backup.
const LabelBackupName = "cnpg.io/backupName"

// MethodVolumeSnapshot and the other method names are CloudNativePG's.
const (
	MethodVolumeSnapshot = "volumeSnapshot"
	MethodBarman         = "barmanObjectStore"
	MethodPlugin         = "plugin"
)

// Options control which objects a plan covers.
type Options struct {
	Namespace   string          // empty: all namespaces
	Now         time.Time       // reference time for the retention window
	Methods     map[string]bool // backup methods to prune; empty: all
	OrphanGrace time.Duration   // orphans younger than this are left alone
}

// Snapshot is one VolumeSnapshot of a backup.
type Snapshot struct {
	Name           string `json:"name"`
	Exists         bool   `json:"exists"`
	Content        string `json:"content,omitempty"`
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// BackupPlan is the decision for one Backup.
type BackupPlan struct {
	Name      string     `json:"name"`
	Method    string     `json:"method"`
	Phase     string     `json:"phase"`
	Time      time.Time  `json:"time"`
	Action    string     `json:"action"`
	Reason    string     `json:"reason"`
	Snapshots []Snapshot `json:"snapshots,omitempty"`
}

// ClusterPlan holds the decisions for the backups of one Cluster.
type ClusterPlan struct {
	Namespace string       `json:"namespace"`
	Name      string       `json:"name"`
	Window    string       `json:"window,omitempty"`
	KeepLast  int          `json:"keepLast,omitempty"`
	Source    string       `json:"source,omitempty"`
	Skipped   string       `json:"skipped,omitempty"`
	Backups   []BackupPlan `json:"backups,omitempty"`
}

// Orphan is a VolumeSnapshot whose Backup is gone, or a volume snapshot
// Backup whose snapshots are all gone.
type Orphan struct {
	Namespace string     `json:"namespace"`
	Kind      string     `json:"kind"`
	Name      string     `json:"name"`
	Reason    string     `json:"reason"`
	Snapshots []Snapshot `json:"snapshots,omitempty"`
}

// Plan is the full result of a dry run.
type Plan struct {
	Now      time.Time     `json:"now"`
	Clusters []ClusterPlan `json:"clusters"`
	Orphans  []Orphan      `json:"orphans"`
}

type snapshotInfo struct {
	created time.Time
	content string
	backup  string
}

// Build reads Clusters, Backups, VolumeSnapshots and VolumeSnapshotContents and
// returns what a retention run would delete. It changes nothing.
func Build(ctx context.Context, client dynamic.Interface, opts Options) (*Plan, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	clusters, err := list(ctx, client, ClusterGVR, opts.Namespace)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	backups, err := list(ctx, client, BackupGVR, opts.Namespace)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	snapshots, err := list(ctx, client, SnapshotGVR, opts.Namespace)
	snapshotAPI := err == nil
	if err != nil && !missingAPI(err) {
		return nil, fmt.Errorf("list volume snapshots: %w", err)
	}
	contents, err := list(ctx, client, ContentGVR, "")
	if err != nil && !missingAPI(err) {
		return nil, fmt.Errorf("list volume snapshot contents: %w", err)
	}

	policies := map[string]string{}
	for _, c := range contents {
		p, _, _ := unstructured.NestedString(c.Object, "spec", "deletionPolicy")
		policies[c.GetName()] = p
	}
	snaps := map[string]snapshotInfo{}
	for _, s := range snapshots {
		content, _, _ := unstructured.NestedString(s.Object, "status", "boundVolumeSnapshotContentName")
		snaps[key(s.GetNamespace(), s.GetName())] = snapshotInfo{
			created: s.GetCreationTimestamp().Time,
			content: content,
			backup:  s.GetLabels()[LabelBackupName],
		}
	}
	describe := func(namespace, name string) Snapshot {
		info, ok := snaps[key(namespace, name)]
		if !ok {
			return Snapshot{Name: name}
		}
		return Snapshot{Name: name, Exists: true, Content: info.content, DeletionPolicy: policies[info.content]}
	}

	byCluster := map[string][]unstructured.Unstructured{}
	backupNames := map[string]bool{}
	for _, b := range backups {
		cluster, _, _ := unstructured.NestedString(b.Object, "spec", "cluster", "name")
		byCluster[key(b.GetNamespace(), cluster)] = append(byCluster[key(b.GetNamespace(), cluster)], b)
		backupNames[key(b.GetNamespace(), b.GetName())] = true
	}

	plan := &Plan{Now: opts.Now}
	for _, c := range clusters {
		cp := ClusterPlan{Namespace: c.GetNamespace(), Name: c.GetName()}
		p, source, skip := clusterPolicy(c)
		cp.Window, cp.KeepLast, cp.Source = p.Window, p.KeepLast, source
		if skip != "" {
			cp.Skipped = skip
			plan.Clusters = append(plan.Clusters, cp)
			continue
		}
		var items []policy.Backup
		byName := map[string]unstructured.Unstructured{}
		for _, b := range byCluster[key(c.GetNamespace(), c.GetName())] {
			if !opts.wants(method(b)) {
				continue
			}
			items = append(items, toPolicy(b))
			byName[b.GetName()] = b
		}
		decisions, err := policy.Decide(items, p, opts.Now)
		if err != nil {
			cp.Skipped = err.Error()
			plan.Clusters = append(plan.Clusters, cp)
			continue
		}
		for _, d := range decisions {
			b := byName[d.Backup.Name]
			bp := BackupPlan{
				Name: d.Backup.Name, Method: d.Backup.Method, Phase: d.Backup.Phase,
				Time: when(d.Backup), Action: string(d.Action), Reason: d.Reason,
			}
			for _, name := range snapshotNames(b) {
				bp.Snapshots = append(bp.Snapshots, describe(b.GetNamespace(), name))
			}
			cp.Backups = append(cp.Backups, bp)
		}
		plan.Clusters = append(plan.Clusters, cp)
	}

	graceStart := opts.Now.Add(-opts.OrphanGrace)
	for _, s := range snapshots {
		info := snaps[key(s.GetNamespace(), s.GetName())]
		if info.backup == "" || backupNames[key(s.GetNamespace(), info.backup)] || !info.created.Before(graceStart) {
			continue
		}
		plan.Orphans = append(plan.Orphans, Orphan{
			Namespace: s.GetNamespace(), Kind: "VolumeSnapshot", Name: s.GetName(),
			Reason:    fmt.Sprintf("its Backup %s no longer exists", info.backup),
			Snapshots: []Snapshot{describe(s.GetNamespace(), s.GetName())},
		})
	}
	for _, b := range backups {
		names := snapshotNames(b)
		if !snapshotAPI || method(b) != MethodVolumeSnapshot || len(names) == 0 || !b.GetCreationTimestamp().Time.Before(graceStart) {
			continue
		}
		missing := 0
		for _, name := range names {
			if _, ok := snaps[key(b.GetNamespace(), name)]; !ok {
				missing++
			}
		}
		if missing == len(names) && !planned(plan, b.GetNamespace(), b.GetName()) {
			plan.Orphans = append(plan.Orphans, Orphan{
				Namespace: b.GetNamespace(), Kind: "Backup", Name: b.GetName(),
				Reason: fmt.Sprintf("none of its %d volume snapshots exist", len(names)),
			})
		}
	}
	sort.Slice(plan.Orphans, func(i, j int) bool {
		a, b := plan.Orphans[i], plan.Orphans[j]
		return a.Namespace+"/"+a.Kind+"/"+a.Name < b.Namespace+"/"+b.Kind+"/"+b.Name
	})
	return plan, nil
}

// Result counts what Apply deleted.
type Result struct {
	Backups   int
	Snapshots int
	Errors    []error
}

// Apply deletes what the plan marks for deletion: first the VolumeSnapshots of
// a backup, then the Backup. Orphans are deleted only when orphans is true.
// Objects that are already gone count as done.
func Apply(ctx context.Context, client dynamic.Interface, plan *Plan, orphans bool) Result {
	var r Result
	deleteSnapshots := func(namespace string, snaps []Snapshot) bool {
		ok := true
		for _, s := range snaps {
			if !s.Exists {
				continue
			}
			if err := remove(ctx, client, SnapshotGVR, namespace, s.Name); err != nil {
				r.Errors = append(r.Errors, fmt.Errorf("delete VolumeSnapshot %s/%s: %w", namespace, s.Name, err))
				ok = false
				continue
			}
			r.Snapshots++
		}
		return ok
	}
	for _, c := range plan.Clusters {
		for _, b := range c.Backups {
			if b.Action != string(policy.Delete) {
				continue
			}
			if !deleteSnapshots(c.Namespace, b.Snapshots) {
				continue // keep the Backup so the next run retries its snapshots
			}
			if err := remove(ctx, client, BackupGVR, c.Namespace, b.Name); err != nil {
				r.Errors = append(r.Errors, fmt.Errorf("delete Backup %s/%s: %w", c.Namespace, b.Name, err))
				continue
			}
			r.Backups++
		}
	}
	if !orphans {
		return r
	}
	for _, o := range plan.Orphans {
		switch o.Kind {
		case "VolumeSnapshot":
			deleteSnapshots(o.Namespace, o.Snapshots)
		case "Backup":
			if err := remove(ctx, client, BackupGVR, o.Namespace, o.Name); err != nil {
				r.Errors = append(r.Errors, fmt.Errorf("delete Backup %s/%s: %w", o.Namespace, o.Name, err))
				continue
			}
			r.Backups++
		}
	}
	return r
}

func (o Options) wants(m string) bool {
	return len(o.Methods) == 0 || o.Methods[m]
}

func clusterPolicy(c unstructured.Unstructured) (policy.Policy, string, string) {
	ann := c.GetAnnotations()
	if v := strings.ToLower(ann[AnnotationSkip]); v == "true" || v == "yes" {
		return policy.Policy{}, "", "annotation " + AnnotationSkip
	}
	var p policy.Policy
	var sources []string
	if v := ann[AnnotationRetention]; v != "" {
		p.Window = v
		sources = append(sources, "retention from annotation")
	} else if v, _, _ := unstructured.NestedString(c.Object, "spec", "backup", "retentionPolicy"); v != "" {
		p.Window = v
		sources = append(sources, "retention from spec.backup.retentionPolicy")
	}
	if v := ann[AnnotationKeepLast]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, "", fmt.Sprintf("annotation %s=%q is not a number", AnnotationKeepLast, v)
		}
		p.KeepLast = n
		sources = append(sources, "keep-last from annotation")
	}
	if p.Window == "" && p.KeepLast == 0 {
		return p, "", "no retention policy (spec.backup.retentionPolicy or " + AnnotationRetention + ")"
	}
	if err := p.Validate(); err != nil {
		return p, "", err.Error()
	}
	return p, strings.Join(sources, ", "), ""
}

func method(b unstructured.Unstructured) string {
	if m, _, _ := unstructured.NestedString(b.Object, "status", "method"); m != "" {
		return m
	}
	if m, _, _ := unstructured.NestedString(b.Object, "spec", "method"); m != "" {
		return m
	}
	return MethodBarman // CloudNativePG's default method
}

func toPolicy(b unstructured.Unstructured) policy.Backup {
	phase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
	out := policy.Backup{Name: b.GetName(), Method: method(b), Phase: phase, Created: b.GetCreationTimestamp().Time}
	if s, _, _ := unstructured.NestedString(b.Object, "status", "stoppedAt"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			out.Stopped = t
		}
	}
	if phase == policy.PhaseCompleted && out.Stopped.IsZero() {
		out.Stopped = out.Created
	}
	return out
}

func snapshotNames(b unstructured.Unstructured) []string {
	elements, _, _ := unstructured.NestedSlice(b.Object, "status", "snapshotBackupStatus", "elements")
	var names []string
	for _, e := range elements {
		if m, ok := e.(map[string]any); ok {
			if n, ok := m["name"].(string); ok && n != "" {
				names = append(names, n)
			}
		}
	}
	return names
}

func planned(p *Plan, namespace, name string) bool {
	for _, c := range p.Clusters {
		if c.Namespace != namespace {
			continue
		}
		for _, b := range c.Backups {
			if b.Name == name && b.Action == string(policy.Delete) {
				return true
			}
		}
	}
	return false
}

func when(b policy.Backup) time.Time {
	if !b.Stopped.IsZero() {
		return b.Stopped
	}
	return b.Created
}

func key(namespace, name string) string { return namespace + "/" + name }

func list(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, namespace string) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured
	opts := metav1.ListOptions{Limit: 500}
	for {
		var l *unstructured.UnstructuredList
		var err error
		if namespace == "" || gvr == ContentGVR {
			l, err = client.Resource(gvr).List(ctx, opts)
		} else {
			l, err = client.Resource(gvr).Namespace(namespace).List(ctx, opts)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, l.Items...)
		if l.GetContinue() == "" {
			return out, nil
		}
		opts.Continue = l.GetContinue()
	}
}

func remove(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, namespace, name string) error {
	policy := metav1.DeletePropagationBackground
	err := client.Resource(gvr).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// missingAPI reports whether the snapshot CRDs are not installed.
func missingAPI(err error) bool {
	return apierrors.IsNotFound(err) || strings.Contains(err.Error(), "the server could not find the requested resource")
}

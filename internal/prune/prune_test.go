package prune

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

var now = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func obj(apiVersion, kind, namespace, name string, created time.Time, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind}}
	for k, v := range fields {
		u.Object[k] = v
	}
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetCreationTimestamp(metav1.NewTime(created))
	return u
}

func cluster(name string, annotations map[string]string, retention string) *unstructured.Unstructured {
	spec := map[string]any{}
	if retention != "" {
		spec["backup"] = map[string]any{"retentionPolicy": retention}
	}
	u := obj("postgresql.cnpg.io/v1", "Cluster", "db", name, now.AddDate(-1, 0, 0), map[string]any{"spec": spec})
	u.SetAnnotations(annotations)
	return u
}

func snapshotBackup(name, cluster string, daysAgo int, snaps ...string) *unstructured.Unstructured {
	stopped := now.AddDate(0, 0, -daysAgo)
	var elements []any
	for _, s := range snaps {
		elements = append(elements, map[string]any{"name": s, "type": "PG_DATA"})
	}
	return obj("postgresql.cnpg.io/v1", "Backup", "db", name, stopped.Add(-time.Minute), map[string]any{
		"spec": map[string]any{"cluster": map[string]any{"name": cluster}, "method": "volumeSnapshot"},
		"status": map[string]any{
			"phase": "completed", "method": "volumeSnapshot", "stoppedAt": stopped.Format(time.RFC3339),
			"snapshotBackupStatus": map[string]any{"elements": elements},
		},
	})
}

func barmanBackup(name, cluster string, daysAgo int) *unstructured.Unstructured {
	stopped := now.AddDate(0, 0, -daysAgo)
	return obj("postgresql.cnpg.io/v1", "Backup", "db", name, stopped.Add(-time.Minute), map[string]any{
		"spec":   map[string]any{"cluster": map[string]any{"name": cluster}},
		"status": map[string]any{"phase": "completed", "stoppedAt": stopped.Format(time.RFC3339)},
	})
}

func volumeSnapshot(name, backup, content string, daysAgo int) *unstructured.Unstructured {
	u := obj("snapshot.storage.k8s.io/v1", "VolumeSnapshot", "db", name, now.AddDate(0, 0, -daysAgo), map[string]any{
		"status": map[string]any{"boundVolumeSnapshotContentName": content},
	})
	u.SetLabels(map[string]string{LabelBackupName: backup, "cnpg.io/cluster": "pg"})
	return u
}

func content(name, deletionPolicy string) *unstructured.Unstructured {
	u := obj("snapshot.storage.k8s.io/v1", "VolumeSnapshotContent", "", name, now, map[string]any{
		"spec": map[string]any{"deletionPolicy": deletionPolicy},
	})
	return u
}

func fakeClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	lists := map[schema.GroupVersionResource]string{
		ClusterGVR: "ClusterList", BackupGVR: "BackupList", SnapshotGVR: "VolumeSnapshotList", ContentGVR: "VolumeSnapshotContentList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, lists, objects...)
}

func names(t *testing.T, c *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource) []string {
	t.Helper()
	l, err := c.Resource(gvr).Namespace("db").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range l.Items {
		out = append(out, i.GetName())
	}
	return out
}

func fixture() *dynamicfake.FakeDynamicClient {
	return fakeClient(
		cluster("pg", map[string]string{AnnotationKeepLast: "1"}, "7d"),
		cluster("other", nil, ""),
		snapshotBackup("b1", "pg", 1, "b1-data", "b1-wal"),
		snapshotBackup("b8", "pg", 8, "b8-data", "b8-wal"),
		snapshotBackup("b9", "pg", 9, "b9-data", "b9-wal"),
		snapshotBackup("b30", "pg", 30, "b30-data"),
		barmanBackup("other-1", "other", 100),
		volumeSnapshot("b1-data", "b1", "c-b1-data", 1), volumeSnapshot("b1-wal", "b1", "c-b1-wal", 1),
		volumeSnapshot("b8-data", "b8", "c-b8-data", 8), volumeSnapshot("b8-wal", "b8", "c-b8-wal", 8),
		volumeSnapshot("b9-data", "b9", "c-b9-data", 9), volumeSnapshot("b9-wal", "b9", "c-b9-wal", 9),
		volumeSnapshot("b30-data", "b30", "c-b30-data", 30),
		volumeSnapshot("gone-data", "gone", "c-gone-data", 3),
		content("c-b1-data", "Delete"), content("c-b1-wal", "Delete"), content("c-b8-data", "Delete"),
		content("c-b8-wal", "Delete"), content("c-b9-data", "Delete"), content("c-b9-wal", "Retain"),
		content("c-b30-data", "Delete"), content("c-gone-data", "Delete"),
	)
}

func TestPlanAndApply(t *testing.T) {
	client := fixture()
	ctx := context.Background()
	plan, err := Build(ctx, client, Options{Now: now, OrphanGrace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	plan.WriteText(&buf)
	text := buf.String()
	t.Log("\n" + text)
	for _, want := range []string{
		"policy: retention 7d, keep-last 1 (retention from spec.backup.retentionPolicy, keep-last from annotation)",
		"KEEP    b8 ", "base backup for the start of the window",
		"DELETE  b9 ", "content c-b9-wal has deletionPolicy Retain",
		"skipped: no retention policy",
		"VolumeSnapshot db/gone-data: its Backup gone no longer exists",
		"Summary: keep 2, delete 2 backups with 3 volume snapshots, 1 orphans",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plan output lacks %q", want)
		}
	}

	res := Apply(ctx, client, plan, false)
	if len(res.Errors) > 0 || res.Backups != 2 || res.Snapshots != 3 {
		t.Fatalf("result %+v", res)
	}
	if got := strings.Join(names(t, client, BackupGVR), ","); got != "b1,b8,other-1" {
		t.Errorf("backups left: %s", got)
	}
	if got := strings.Join(names(t, client, SnapshotGVR), ","); got != "b1-data,b1-wal,b8-data,b8-wal,gone-data" {
		t.Errorf("snapshots left: %s", got)
	}

	plan, err = Build(ctx, client, Options{Now: now, OrphanGrace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	res = Apply(ctx, client, plan, true)
	if len(res.Errors) > 0 || res.Backups != 0 || res.Snapshots != 1 {
		t.Fatalf("orphan result %+v", res)
	}
	if got := strings.Join(names(t, client, SnapshotGVR), ","); got != "b1-data,b1-wal,b8-data,b8-wal" {
		t.Errorf("snapshots left after orphans: %s", got)
	}
}

func TestAnnotationOverridesAndSkip(t *testing.T) {
	client := fakeClient(
		cluster("pg", map[string]string{AnnotationRetention: "3d"}, "30d"),
		cluster("skip", map[string]string{AnnotationSkip: "true"}, "1d"),
		cluster("bad", map[string]string{AnnotationKeepLast: "many"}, ""),
		snapshotBackup("p1", "pg", 1), snapshotBackup("p5", "pg", 5), snapshotBackup("p6", "pg", 6),
		snapshotBackup("s1", "skip", 10), snapshotBackup("s2", "skip", 20),
	)
	plan, err := Build(context.Background(), client, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ClusterPlan{}
	for _, c := range plan.Clusters {
		byName[c.Name] = c
	}
	if c := byName["pg"]; c.Window != "3d" || len(c.Backups) != 3 || c.Backups[2].Action != "delete" {
		t.Errorf("pg plan %+v", c)
	}
	if c := byName["skip"]; c.Skipped == "" || len(c.Backups) != 0 {
		t.Errorf("skip plan %+v", c)
	}
	if c := byName["bad"]; !strings.Contains(c.Skipped, "not a number") {
		t.Errorf("bad plan %+v", c)
	}
}

func TestMethodFilterAndDanglingBackup(t *testing.T) {
	client := fakeClient(
		cluster("pg", nil, "7d"),
		snapshotBackup("snap-new", "pg", 1, "snap-new-data"),
		snapshotBackup("snap-old", "pg", 20, "snap-old-data"),
		barmanBackup("barman-old", "pg", 30),
		volumeSnapshot("snap-new-data", "snap-new", "c1", 1),
		// snap-old's snapshot was deleted out of band
	)
	plan, err := Build(context.Background(), client, Options{Now: now, Methods: map[string]bool{MethodVolumeSnapshot: true}, OrphanGrace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, b := range plan.Clusters[0].Backups {
		actions[b.Name] = b.Action
	}
	if _, ok := actions["barman-old"]; ok {
		t.Errorf("barman backup was planned despite the method filter: %v", actions)
	}
	// snap-old is the base backup for the window, so it is kept by the policy
	// and reported as an orphan because its snapshot is gone.
	if actions["snap-old"] != "keep" || len(plan.Orphans) != 1 || plan.Orphans[0].Name != "snap-old" {
		t.Fatalf("actions %v orphans %+v", actions, plan.Orphans)
	}
}

func TestMissingSnapshotAPI(t *testing.T) {
	client := fakeClient(cluster("pg", nil, "7d"), barmanBackup("a", "pg", 1), barmanBackup("b", "pg", 20), barmanBackup("c", "pg", 30))
	for _, resource := range []string{"volumesnapshots", "volumesnapshotcontents"} {
		gr := schema.GroupResource{Group: "snapshot.storage.k8s.io", Resource: resource}
		client.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(gr, "")
		})
	}
	plan, err := Build(context.Background(), client, Options{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if n := plan.Count(); n.Deleted != 1 || n.Kept != 2 || n.Orphans != 0 {
		t.Fatalf("counts %+v", n)
	}
}

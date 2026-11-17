package policy

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func daysAgo(d int) time.Time { return now.AddDate(0, 0, -d) }

func completed(name string, d int) Backup {
	return Backup{Name: name, Phase: PhaseCompleted, Created: daysAgo(d).Add(-time.Minute), Stopped: daysAgo(d)}
}

func actions(t *testing.T, ds []Decision) map[string]Action {
	t.Helper()
	out := map[string]Action{}
	for _, d := range ds {
		out[d.Backup.Name] = d.Action
	}
	return out
}

func TestCutoff(t *testing.T) {
	cases := map[string]time.Time{
		"30d": time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
		"2w":  time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		"1m":  time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := Cutoff(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("Cutoff(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0d", "30", "30h", "-1d", "1y"} {
		if _, err := Cutoff(bad, now); err == nil {
			t.Errorf("Cutoff(%q) accepted", bad)
		}
	}
}

func TestWindowKeepsBaseBackup(t *testing.T) {
	backups := []Backup{
		completed("d1", 1), completed("d5", 5), completed("d8", 8), completed("d9", 9), completed("d20", 20),
	}
	ds, err := Decide(backups, Policy{Window: "7d"}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := actions(t, ds)
	want := map[string]Action{"d1": Keep, "d5": Keep, "d8": Keep, "d9": Delete, "d20": Delete}
	for name, a := range want {
		if got[name] != a {
			t.Errorf("%s: got %s, want %s", name, got[name], a)
		}
	}
	if ds[2].Reason != "base backup for the start of the window" {
		t.Errorf("d8 reason = %q", ds[2].Reason)
	}
}

func TestNewestIsAlwaysKept(t *testing.T) {
	ds, err := Decide([]Backup{completed("old", 100), completed("older", 200)}, Policy{Window: "7d"}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := actions(t, ds)
	if got["old"] != Keep || got["older"] != Delete {
		t.Fatalf("got %v", got)
	}
}

func TestKeepLastWithoutWindow(t *testing.T) {
	backups := []Backup{completed("a", 1), completed("b", 2), completed("c", 3), completed("d", 4)}
	ds, err := Decide(backups, Policy{KeepLast: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := actions(t, ds)
	if got["a"] != Keep || got["b"] != Keep || got["c"] != Delete || got["d"] != Delete {
		t.Fatalf("got %v", got)
	}
}

func TestKeepLastOverridesWindow(t *testing.T) {
	backups := []Backup{completed("a", 30), completed("b", 31), completed("c", 32), completed("d", 33)}
	ds, err := Decide(backups, Policy{Window: "7d", KeepLast: 3}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := actions(t, ds)
	if got["a"] != Keep || got["b"] != Keep || got["c"] != Keep || got["d"] != Delete {
		t.Fatalf("got %v", got)
	}
}

func TestFailedAndRunning(t *testing.T) {
	backups := []Backup{
		completed("ok", 1),
		{Name: "failed-new", Phase: PhaseFailed, Created: daysAgo(2)},
		{Name: "failed-old", Phase: PhaseFailed, Created: daysAgo(10)},
		{Name: "running", Phase: "running", Created: daysAgo(20)},
		{Name: "pending", Phase: "", Created: daysAgo(30)},
	}
	ds, err := Decide(backups, Policy{Window: "7d"}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := actions(t, ds)
	want := map[string]Action{"ok": Keep, "failed-new": Keep, "failed-old": Delete, "running": Keep, "pending": Keep}
	for name, a := range want {
		if got[name] != a {
			t.Errorf("%s: got %s, want %s", name, got[name], a)
		}
	}
	for _, d := range ds {
		if d.Backup.Name == "pending" && !strings.Contains(d.Reason, "no phase yet") {
			t.Errorf("pending reason = %q", d.Reason)
		}
	}
}

func TestFailedWithoutWindow(t *testing.T) {
	backups := []Backup{
		{Name: "failed-before", Phase: PhaseFailed, Created: daysAgo(3)},
		completed("ok", 2),
		{Name: "failed-after", Phase: PhaseFailed, Created: daysAgo(1)},
	}
	ds, err := Decide(backups, Policy{KeepLast: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := actions(t, ds)
	if got["failed-before"] != Delete || got["failed-after"] != Keep || got["ok"] != Keep {
		t.Fatalf("got %v", got)
	}
}

func TestInvalidPolicy(t *testing.T) {
	for _, p := range []Policy{{}, {KeepLast: -1, Window: "7d"}, {Window: "7x"}} {
		if _, err := Decide(nil, p, now); err == nil {
			t.Errorf("policy %+v accepted", p)
		}
	}
}

// Package policy decides which CloudNativePG backups a retention policy keeps.
//
// The rules follow the recovery-window semantics Barman applies to object-store
// backups: every backup that finished inside the window is kept, and so is the
// newest backup that finished before the window started, because it is the base
// for a restore to the start of the window.
package policy

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// Backup phases as written by CloudNativePG (api/v1/backup_types.go).
const (
	PhaseCompleted = "completed"
	PhaseFailed    = "failed"
	PhaseInvalid   = "invalid backup definition"
)

// Backup is the part of a Backup resource the policy needs.
type Backup struct {
	Name    string
	Method  string
	Phase   string
	Created time.Time
	Stopped time.Time // zero unless the backup finished
}

// Policy is a retention policy for the backups of one cluster.
type Policy struct {
	// Window is a CloudNativePG retention policy such as "30d", "4w" or "6m".
	// Empty means no age limit.
	Window string
	// KeepLast keeps the newest N completed backups regardless of age.
	KeepLast int
}

// Action is what happens to a backup.
type Action string

const (
	Keep   Action = "keep"
	Delete Action = "delete"
)

// Decision is the outcome for one backup.
type Decision struct {
	Backup Backup
	Action Action
	Reason string
}

var windowPattern = regexp.MustCompile(`^([1-9][0-9]*)([dwm])$`)

// Cutoff returns the start of the recovery window for a CloudNativePG
// retention policy string ("30d", "4w", "6m"), relative to now.
func Cutoff(window string, now time.Time) (time.Time, error) {
	m := windowPattern.FindStringSubmatch(window)
	if m == nil {
		return time.Time{}, fmt.Errorf("retention policy %q: want a number followed by d, w or m, such as 30d", window)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("retention policy %q: %w", window, err)
	}
	switch m[2] {
	case "d":
		return now.AddDate(0, 0, -n), nil
	case "w":
		return now.AddDate(0, 0, -7*n), nil
	default:
		return now.AddDate(0, -n, 0), nil
	}
}

// Validate reports whether the policy can be applied.
func (p Policy) Validate() error {
	if p.KeepLast < 0 {
		return fmt.Errorf("keep-last must not be negative, got %d", p.KeepLast)
	}
	if p.Window != "" {
		if _, err := Cutoff(p.Window, time.Now()); err != nil {
			return err
		}
	}
	if p.Window == "" && p.KeepLast == 0 {
		return fmt.Errorf("policy has neither a retention window nor keep-last")
	}
	return nil
}

// Decide returns one decision per backup, newest first.
//
// Completed backups are kept when they are among the newest KeepLast, finished
// inside the window, or are the newest one that finished before the window.
// The newest completed backup is always kept. Failed and invalid backups are
// deleted once they were created before the window start (or, without a
// window, when they are older than the newest completed backup). Backups that
// are still running are never touched.
func Decide(backups []Backup, p Policy, now time.Time) ([]Decision, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var cutoff time.Time
	if p.Window != "" {
		cutoff, _ = Cutoff(p.Window, now)
	}

	sorted := append([]Backup(nil), backups...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return when(sorted[i]).After(when(sorted[j]))
	})

	var newestCompleted time.Time
	for _, b := range sorted {
		if b.Phase == PhaseCompleted {
			newestCompleted = b.Stopped
			break
		}
	}

	out := make([]Decision, 0, len(sorted))
	completed := 0
	baseKept := false
	for _, b := range sorted {
		d := Decision{Backup: b, Action: Keep}
		switch b.Phase {
		case PhaseCompleted:
			completed++
			switch {
			case completed == 1:
				d.Reason = "newest completed backup"
			case completed <= p.KeepLast:
				d.Reason = fmt.Sprintf("among the newest %d", p.KeepLast)
			case p.Window != "" && !b.Stopped.Before(cutoff):
				d.Reason = "inside the " + p.Window + " window"
			case p.Window != "" && !baseKept:
				d.Reason = "base backup for the start of the window"
			case p.Window == "":
				d.Action, d.Reason = Delete, fmt.Sprintf("beyond the newest %d", p.KeepLast)
			default:
				d.Action, d.Reason = Delete, "finished before the "+p.Window+" window"
			}
			if p.Window != "" && b.Stopped.Before(cutoff) && d.Action == Keep {
				baseKept = true
			}
		case PhaseFailed, PhaseInvalid:
			switch {
			case p.Window != "" && b.Created.Before(cutoff):
				d.Action, d.Reason = Delete, b.Phase+", created before the "+p.Window+" window"
			case p.Window == "" && !newestCompleted.IsZero() && b.Created.Before(newestCompleted):
				d.Action, d.Reason = Delete, b.Phase+", older than the newest completed backup"
			default:
				d.Reason = b.Phase + ", kept for inspection"
			}
		default:
			phase := b.Phase
			if phase == "" {
				phase = "no phase yet"
			}
			d.Reason = "in progress (" + phase + ")"
		}
		out = append(out, d)
	}
	return out, nil
}

func when(b Backup) time.Time {
	if !b.Stopped.IsZero() {
		return b.Stopped
	}
	return b.Created
}

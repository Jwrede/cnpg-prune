package prune

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Counts summarises a plan.
type Counts struct {
	Kept, Deleted, Snapshots, Orphans int
}

// Count returns how many backups the plan keeps and deletes, how many
// VolumeSnapshots go with the deleted backups, and how many orphans it found.
func (p *Plan) Count() Counts {
	var c Counts
	for _, cp := range p.Clusters {
		for _, b := range cp.Backups {
			if b.Action == "delete" {
				c.Deleted++
				for _, s := range b.Snapshots {
					if s.Exists {
						c.Snapshots++
					}
				}
			} else {
				c.Kept++
			}
		}
	}
	c.Orphans = len(p.Orphans)
	return c
}

// WriteText prints the plan for people.
func (p *Plan) WriteText(w io.Writer) {
	for _, c := range p.Clusters {
		fmt.Fprintf(w, "Cluster %s/%s\n", c.Namespace, c.Name)
		if c.Skipped != "" {
			fmt.Fprintf(w, "  skipped: %s\n\n", c.Skipped)
			continue
		}
		var parts []string
		if c.Window != "" {
			parts = append(parts, "retention "+c.Window)
		}
		if c.KeepLast > 0 {
			parts = append(parts, fmt.Sprintf("keep-last %d", c.KeepLast))
		}
		fmt.Fprintf(w, "  policy: %s (%s)\n", strings.Join(parts, ", "), c.Source)
		if len(c.Backups) == 0 {
			fmt.Fprintln(w, "  no backups")
		}
		for _, b := range c.Backups {
			fmt.Fprintf(w, "  %-6s  %-30s  %-17s  %-10s  %s  %s\n",
				strings.ToUpper(b.Action), b.Name, b.Method, b.Phase, b.Time.UTC().Format(time.RFC3339), b.Reason)
			if b.Action == "delete" {
				for _, s := range b.Snapshots {
					fmt.Fprintf(w, "          VolumeSnapshot %s: %s\n", s.Name, snapshotNote(s))
				}
			}
		}
		fmt.Fprintln(w)
	}
	if len(p.Orphans) > 0 {
		fmt.Fprintln(w, "Orphans (deleted only with -orphans)")
		for _, o := range p.Orphans {
			fmt.Fprintf(w, "  %s %s/%s: %s\n", o.Kind, o.Namespace, o.Name, o.Reason)
			for _, s := range o.Snapshots {
				fmt.Fprintf(w, "          %s\n", snapshotNote(s))
			}
		}
		fmt.Fprintln(w)
	}
	n := p.Count()
	fmt.Fprintf(w, "Summary: keep %d, delete %d backups with %d volume snapshots, %d orphans\n",
		n.Kept, n.Deleted, n.Snapshots, n.Orphans)
}

func snapshotNote(s Snapshot) string {
	switch {
	case !s.Exists:
		return "already gone"
	case s.Content == "":
		return "not bound to a VolumeSnapshotContent"
	case s.DeletionPolicy == "Retain":
		return fmt.Sprintf("content %s has deletionPolicy Retain, the storage snapshot stays", s.Content)
	case s.DeletionPolicy == "":
		return fmt.Sprintf("content %s (deletionPolicy unknown)", s.Content)
	default:
		return fmt.Sprintf("content %s is deleted with it (deletionPolicy %s)", s.Content, s.DeletionPolicy)
	}
}

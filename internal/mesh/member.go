// Package mesh holds the membership model: member records and their merge
// rules, invites, one-time pairing tokens and local address detection.
package mesh

import (
	"slices"
	"sort"
	"strconv"
)

type Member struct {
	ID         string
	Name       string
	Addrs      []string
	Generation uint64
	Removed    bool
	// Epoch counts removals and re-pairings. It outranks Generation, so an
	// owner's later changes (which raise only Generation) can never pass a
	// tombstone; only a new pairing can.
	Epoch uint64
}

// ReservedName is never a valid host name; it denotes the local host.
const ReservedName = "local"

// Merge folds remote into local and reports whether local changed. The
// result is sorted by ID and does not alias either input. Rules: higher
// Epoch wins, then the larger Generation; on a tie Removed wins; two live
// records of equal Epoch and Generation union their addresses; remote changes
// to selfID are ignored.
func Merge(local, remote []Member, selfID string) (merged []Member, changed bool) {
	byID := make(map[string]Member, len(local)+len(remote))
	localByID := make(map[string]Member, len(local))
	for _, m := range local {
		m = clone(m)
		byID[m.ID] = m
		localByID[m.ID] = m
	}
	for _, r := range remote {
		if r.ID == selfID {
			continue
		}
		cur, ok := byID[r.ID]
		if !ok {
			byID[r.ID] = clone(r)
			continue
		}
		byID[r.ID] = mergeOne(cur, r)
	}
	merged = make([]Member, 0, len(byID))
	for _, m := range byID {
		merged = append(merged, m)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })

	if len(merged) != len(localByID) {
		return merged, true
	}
	for _, m := range merged {
		if !sameMember(m, localByID[m.ID]) {
			return merged, true
		}
	}
	return merged, false
}

func mergeOne(a, b Member) Member {
	switch {
	case a.Epoch > b.Epoch:
		return a
	case b.Epoch > a.Epoch:
		return clone(b)
	case a.Generation > b.Generation:
		return a
	case b.Generation > a.Generation:
		return clone(b)
	case a.Removed && !b.Removed:
		return a
	case b.Removed && !a.Removed:
		return clone(b)
	}
	a.Addrs = union(a.Addrs, b.Addrs)
	return a
}

func union(a, b []string) []string {
	all := append(slices.Clone(a), b...)
	slices.Sort(all)
	return slices.Compact(all)
}

func clone(m Member) Member {
	m.Addrs = slices.Clone(m.Addrs)
	return m
}

func sameMember(a, b Member) bool {
	if a.ID != b.ID || a.Epoch != b.Epoch || a.Name != b.Name || a.Generation != b.Generation || a.Removed != b.Removed {
		return false
	}
	return slices.Equal(union(a.Addrs, nil), union(b.Addrs, nil))
}

// LocalNames maps member ID to the alias this host uses for it. Only active
// members get an alias. Members are visited in ID order; a name already taken
// (or the reserved "local") gets the next free "-2", "-3", ... suffix.
func LocalNames(members []Member, selfID string) map[string]string {
	active := make([]Member, 0, len(members))
	for _, m := range members {
		if !m.Removed {
			active = append(active, m)
		}
	}
	// This host takes its own name first, so no other member can shadow it;
	// the rest are allotted in ID order.
	sort.Slice(active, func(i, j int) bool {
		si, sj := active[i].ID == selfID, active[j].ID == selfID
		if si != sj {
			return si
		}
		return active[i].ID < active[j].ID
	})

	out := make(map[string]string, len(active))
	used := map[string]bool{ReservedName: true}
	for _, m := range active {
		name := m.Name
		for n := 2; used[name]; n++ {
			name = m.Name + "-" + strconv.Itoa(n)
		}
		used[name] = true
		out[m.ID] = name
	}
	return out
}

// ValidName reports whether name matches [a-z0-9-]{1,32} and is not reserved.
func ValidName(name string) bool {
	if len(name) < 1 || len(name) > 32 || name == ReservedName {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

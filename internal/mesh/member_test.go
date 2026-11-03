package mesh

import (
	"math/rand"
	"reflect"
	"testing"
)

func find(ms []Member, id string) (Member, bool) {
	for _, m := range ms {
		if m.ID == id {
			return m, true
		}
	}
	return Member{}, false
}

func TestMergeHigherGenerationWins(t *testing.T) {
	local := []Member{{ID: "a", Name: "old", Addrs: []string{"1:1"}, Generation: 1}}
	remote := []Member{{ID: "a", Name: "new", Addrs: []string{"2:2"}, Generation: 2}}
	got, changed := Merge(local, remote, "self")
	m, _ := find(got, "a")
	if !changed || m.Name != "new" || !reflect.DeepEqual(m.Addrs, []string{"2:2"}) {
		t.Fatalf("%+v changed=%v", got, changed)
	}
	got, changed = Merge(remote, local, "self")
	if changed {
		t.Fatalf("lower generation changed local: %+v", got)
	}
}

func TestMergeRemovedWinsTie(t *testing.T) {
	live := []Member{{ID: "a", Name: "a", Generation: 2}}
	dead := []Member{{ID: "a", Name: "a", Generation: 2, Removed: true}}
	got, changed := Merge(live, dead, "self")
	if m, _ := find(got, "a"); !m.Removed || !changed {
		t.Fatalf("%+v %v", got, changed)
	}
	got, changed = Merge(dead, live, "self")
	if m, _ := find(got, "a"); !m.Removed || changed {
		t.Fatalf("%+v %v", got, changed)
	}
}

func TestMergeOldLiveCannotReviveTombstone(t *testing.T) {
	local := []Member{{ID: "a", Name: "a", Generation: 3, Removed: true}}
	remote := []Member{{ID: "a", Name: "a", Generation: 2}}
	got, changed := Merge(local, remote, "self")
	if m, _ := find(got, "a"); !m.Removed || changed {
		t.Fatalf("%+v %v", got, changed)
	}
}

func TestMergeUnionAddrsOnTie(t *testing.T) {
	local := []Member{{ID: "a", Name: "a", Addrs: []string{"1:1", "2:2"}, Generation: 1}}
	remote := []Member{{ID: "a", Name: "a", Addrs: []string{"2:2", "3:3"}, Generation: 1}}
	got, changed := Merge(local, remote, "self")
	m, _ := find(got, "a")
	if !changed || !reflect.DeepEqual(m.Addrs, []string{"1:1", "2:2", "3:3"}) {
		t.Fatalf("%+v %v", got, changed)
	}
	_, changed = Merge(got, remote, "self")
	if changed {
		t.Fatal("subset addrs reported changed")
	}
}

func TestMergeAddsUnknownMembers(t *testing.T) {
	got, changed := Merge(nil, []Member{{ID: "b", Name: "b", Generation: 1}}, "self")
	if !changed || len(got) != 1 {
		t.Fatalf("%+v %v", got, changed)
	}
}

func TestMergeIgnoresRemoteSelf(t *testing.T) {
	local := []Member{{ID: "self", Name: "me", Addrs: []string{"1:1"}, Generation: 1}}
	remote := []Member{{ID: "self", Name: "evil", Addrs: []string{"9:9"}, Generation: 99, Removed: true}}
	got, changed := Merge(local, remote, "self")
	if changed || !reflect.DeepEqual(got, local) {
		t.Fatalf("%+v %v", got, changed)
	}
	// Remote self is not even adopted when local lacks it.
	got, _ = Merge(nil, remote, "self")
	if len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestMergeDoesNotAliasInputs(t *testing.T) {
	local := []Member{{ID: "a", Addrs: []string{"1:1"}, Generation: 1}}
	got, _ := Merge(local, nil, "self")
	got[0].Addrs[0] = "x"
	if local[0].Addrs[0] != "1:1" {
		t.Fatal("aliased")
	}
}

func TestMergeConverges(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	ids := []string{"a", "b", "c", "d"}
	mk := func() []Member {
		var l []Member
		for _, id := range ids {
			if r.Intn(4) == 0 {
				continue
			}
			g := uint64(r.Intn(3))
			m := Member{ID: id, Name: id, Generation: g, Epoch: uint64(r.Intn(3)), Removed: r.Intn(3) == 0}
			for _, a := range []string{"1:1", "2:2", "3:3"} {
				if r.Intn(2) == 0 {
					m.Addrs = append(m.Addrs, a)
				}
			}
			l = append(l, m)
		}
		return l
	}
	for i := 0; i < 200; i++ {
		x, y, z := mk(), mk(), mk()
		var results [][]Member
		for _, order := range [][]([]Member){{x, y, z}, {z, y, x}, {y, z, x}, {x, z, y}} {
			acc := order[0]
			for _, o := range order[1:] {
				acc, _ = Merge(acc, o, "none")
			}
			results = append(results, acc)
		}
		for _, res := range results[1:] {
			if !reflect.DeepEqual(norm(results[0]), norm(res)) {
				t.Fatalf("diverged:\n%+v\n%+v", results[0], res)
			}
		}
	}
}

func norm(ms []Member) []Member {
	if ms == nil {
		return []Member{}
	}
	return ms
}

func TestLocalNamesDedup(t *testing.T) {
	ms := []Member{
		{ID: "b", Name: "nas"},
		{ID: "a", Name: "nas"},
		{ID: "c", Name: "local"},
		{ID: "d", Name: "gone", Removed: true},
		{ID: "e", Name: "nas"},
	}
	got := LocalNames(ms, "a")
	want := map[string]string{"a": "nas", "b": "nas-2", "e": "nas-3", "c": "local-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestValidName(t *testing.T) {
	for n, want := range map[string]bool{
		"nas": true, "a-1": true, "": false, "Nas": false, "a_b": false, "local": false,
		"한글": false, "a b": false,
		"abcdefghijklmnopqrstuvwxyz012345":  true,
		"abcdefghijklmnopqrstuvwxyz0123456": false,
	} {
		if ValidName(n) != want {
			t.Errorf("ValidName(%q) != %v", n, want)
		}
	}
}

func TestMergeEpochOutranksGeneration(t *testing.T) {
	tomb := []Member{{ID: "a", Name: "a", Generation: 2, Removed: true, Epoch: 1}}
	busy := []Member{{ID: "a", Name: "renamed", Generation: 9, Epoch: 0}}
	for name, c := range map[string]struct{ local, remote []Member }{"tombstone local": {tomb, busy}, "tombstone remote": {busy, tomb}} {
		got, _ := Merge(c.local, c.remote, "self")
		if m, _ := find(got, "a"); !m.Removed || m.Epoch != 1 {
			t.Errorf("%s: %+v", name, got)
		}
	}
	// A new pairing (higher epoch, lower generation) revives.
	back := []Member{{ID: "a", Name: "a", Generation: 1, Epoch: 2}}
	got, changed := Merge(tomb, back, "self")
	if m, _ := find(got, "a"); m.Removed || !changed {
		t.Fatalf("%+v %v", got, changed)
	}
	got, changed = Merge(back, tomb, "self")
	if m, _ := find(got, "a"); m.Removed || changed {
		t.Fatalf("%+v %v", got, changed)
	}
	// Equal epochs fall back to the generation rules.
	got, _ = Merge([]Member{{ID: "a", Name: "x", Generation: 1, Epoch: 1}}, []Member{{ID: "a", Name: "y", Generation: 2, Epoch: 1}}, "self")
	if m, _ := find(got, "a"); m.Name != "y" {
		t.Fatalf("%+v", got)
	}
	// An epoch-only difference counts as a change.
	if _, changed := Merge([]Member{{ID: "a", Name: "a", Generation: 1}}, []Member{{ID: "a", Name: "a", Generation: 1, Epoch: 1}}, "self"); !changed {
		t.Fatal("epoch change not reported")
	}
}

func TestLocalNamesSelfFirst(t *testing.T) {
	ms := []Member{{ID: "aaa", Name: "nas"}, {ID: "zzz", Name: "nas"}}
	got := LocalNames(ms, "zzz")
	if got["zzz"] != "nas" || got["aaa"] != "nas-2" {
		t.Fatalf("%v", got)
	}
}

package centralstore

import "testing"

// lineageGraph builds a Lineage lookup from a parent map. A node is present if
// it appears as a key; hasParent is true only when its entry is non-empty.
func lineageGraph(edges map[string]string) func(string) (string, bool, bool) {
	return func(id string) (string, bool, bool) {
		parent, present := edges[id]
		return parent, parent != "", present
	}
}

func TestLineageLinearChain(t *testing.T) {
	// a(root) ← b ← c
	g := lineageGraph(map[string]string{"a": "", "b": "a", "c": "b"})
	for id, want := range map[string]struct {
		root  string
		depth int
	}{"a": {"a", 0}, "b": {"a", 1}, "c": {"a", 2}} {
		root, depth := Lineage(g, id)
		if root != want.root || depth != want.depth {
			t.Errorf("Lineage(%q) = (%q,%d), want (%q,%d)", id, root, depth, want.root, want.depth)
		}
	}
}

func TestLineageBranch(t *testing.T) {
	// root ← d1 ← d1a ; root ← d2
	g := lineageGraph(map[string]string{"root": "", "d1": "root", "d1a": "d1", "d2": "root"})
	for id, want := range map[string]struct {
		root  string
		depth int
	}{"root": {"root", 0}, "d1": {"root", 1}, "d1a": {"root", 2}, "d2": {"root", 1}} {
		root, depth := Lineage(g, id)
		if root != want.root || depth != want.depth {
			t.Errorf("Lineage(%q) = (%q,%d), want (%q,%d)", id, root, depth, want.root, want.depth)
		}
	}
}

// A parent outside the graph (a foreign/peer id, or a dismissed/absent row)
// makes the last present node its own root — matching family presentation and
// the budget. This is the case the old durable walk got wrong (depth 1).
func TestLineageDanglingParentIsOwnRoot(t *testing.T) {
	// "orphan" has a parent edge to "ghost", which is absent from the graph.
	g := lineageGraph(map[string]string{"orphan": "ghost", "child": "orphan"})
	if root, depth := Lineage(g, "orphan"); root != "orphan" || depth != 0 {
		t.Errorf("dangling orphan = (%q,%d), want (orphan,0)", root, depth)
	}
	if root, depth := Lineage(g, "child"); root != "orphan" || depth != 1 {
		t.Errorf("child of dangling = (%q,%d), want (orphan,1)", root, depth)
	}
}

// A 2-cycle collapses onto the lexicographically smallest member. The walk to
// the resolved root is bounded; the root arm is 0 and the arm that reaches the
// root in one hop is 1, exactly as the budget counts them.
func TestLineageCycleCollapses(t *testing.T) {
	g := lineageGraph(map[string]string{"x": "y", "y": "x"})
	if root, depth := Lineage(g, "x"); root != "x" || depth != 0 {
		t.Errorf("cycle root arm = (%q,%d), want (x,0)", root, depth)
	}
	if root, depth := Lineage(g, "y"); root != "x" || depth != 1 {
		t.Errorf("cycle other arm = (%q,%d), want (x,1)", root, depth)
	}
}

func TestLineageMissingStart(t *testing.T) {
	g := lineageGraph(map[string]string{"a": ""})
	if root, depth := Lineage(g, "absent"); root != "" || depth != 0 {
		t.Errorf("Lineage(absent) = (%q,%d), want (\"\",0)", root, depth)
	}
}

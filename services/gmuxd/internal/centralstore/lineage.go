package centralstore

// Lineage resolves a session's behavioral root and depth within a parent
// graph supplied through lookup, independent of how the graph is stored.
//
// It is the single authoritative definition of lineage depth, shared by the
// active-subagent budget (enforcement: MaxSubagentsByDepth, reservation
// receipts) and the session wire projection (the "depth" field on
// /v1/sessions). Both callers adapt their own graph representation to the
// lookup callback, so a single implementation backs every consumer-facing
// notion of "how deep is this session."
//
// Semantics (deliberately identical to the budget's historical walk):
//
//   - Depth is root-relative: a root is 0, each parent hop adds 1.
//   - A missing parent row makes the last present node its own root, matching
//     family presentation. A session whose parent is a foreign/peer id it
//     cannot see is therefore a root at depth 0.
//   - Corrupt cycles are collapsed onto their lexicographically smallest
//     member so resolution stays bounded and deterministic instead of looping
//     or splitting one cycle across unrelated roots. A node whose parent walk
//     cannot reach the resolved root (the cycle arm itself) reports depth 0.
//   - A start that is itself absent resolves to the empty root and depth 0.
//
// lookup(id) returns the node's parent id, whether it has a parent edge at
// all, and whether the node is present in the graph.
func Lineage(lookup func(id string) (parent string, hasParent, present bool), start string) (root string, depth int) {
	root = resolveLineageRoot(lookup, start)
	if root == "" {
		return "", 0
	}
	depth = 0
	cur := start
	seen := make(map[string]bool)
	for cur != root {
		if seen[cur] {
			return root, 0
		}
		seen[cur] = true
		parent, hasParent, present := lookup(cur)
		if !present || !hasParent {
			return root, 0
		}
		cur = parent
		depth++
	}
	return root, depth
}

// resolveLineageRoot follows parent edges to the behavioral root, applying the
// missing-parent and cycle rules documented on Lineage.
func resolveLineageRoot(lookup func(id string) (parent string, hasParent, present bool), start string) string {
	path := make([]string, 0, 8)
	seen := make(map[string]int)
	cur := start
	for {
		parent, hasParent, present := lookup(cur)
		if !present {
			if len(path) == 0 {
				return ""
			}
			return path[len(path)-1]
		}
		if !hasParent {
			return cur
		}
		seen[cur] = len(path)
		path = append(path, cur)
		if at, cycle := seen[parent]; cycle {
			root := parent
			for _, id := range path[at:] {
				if id < root {
					root = id
				}
			}
			return root
		}
		if _, _, ok := lookup(parent); !ok {
			return cur
		}
		cur = parent
	}
}

package asset

import (
	"testing"

	"github.com/google/uuid"
)

// chain builds employee → identity → group → app → server with one stray asset.
func chain() (map[NodeID]NodeData, []Edge, []NodeID) {
	ids := []NodeID{
		MakeNodeID("employee", uuid.MustParse("00000000-0000-7000-8000-000000000001")),
		MakeNodeID("identity", uuid.MustParse("00000000-0000-7000-8000-000000000002")),
		MakeNodeID("asset", uuid.MustParse("00000000-0000-7000-8000-000000000003")),
		MakeNodeID("asset", uuid.MustParse("00000000-0000-7000-8000-000000000004")),
		MakeNodeID("asset", uuid.MustParse("00000000-0000-7000-8000-000000000005")),
		MakeNodeID("asset", uuid.MustParse("00000000-0000-7000-8000-000000000006")), // isolated
	}
	nodes := map[NodeID]NodeData{}
	for _, id := range ids {
		nodes[id] = NodeData{ID: id, Label: string(id)}
	}
	edges := []Edge{
		{ID: "e1", Type: "HAS_IDENTITY", Source: ids[0], Target: ids[1]},
		{ID: "e2", Type: "MEMBER_OF", Source: ids[1], Target: ids[2]},
		{ID: "e3", Type: "HAS_ACCESS_TO", Source: ids[2], Target: ids[3]},
		{ID: "e4", Type: "HOSTED_ON", Source: ids[3], Target: ids[4]},
	}
	return nodes, edges, ids
}

func nodeSet(g Graph) map[NodeID]bool {
	m := map[NodeID]bool{}
	for _, n := range g.Nodes {
		m[n.Data.ID] = true
	}
	return m
}

func TestBuildGraphFull(t *testing.T) {
	nodes, edges, _ := chain()
	g := BuildGraph(nodes, edges, nil, 2, 100)
	if len(g.Nodes) != 6 || len(g.Edges) != 4 || g.Truncated {
		t.Fatalf("full graph: %d nodes, %d edges, truncated=%v", len(g.Nodes), len(g.Edges), g.Truncated)
	}
}

func TestBuildGraphDepthFromRoot(t *testing.T) {
	nodes, edges, ids := chain()

	g := BuildGraph(nodes, edges, &ids[0], 2, 100)
	set := nodeSet(g)
	if len(set) != 3 || !set[ids[0]] || !set[ids[1]] || !set[ids[2]] {
		t.Fatalf("depth 2 from employee: %v", set)
	}
	if len(g.Edges) != 2 {
		t.Fatalf("edges must connect selected nodes only, got %d", len(g.Edges))
	}

	// Traversal is undirected: from the server we reach the app and group.
	g = BuildGraph(nodes, edges, &ids[4], 2, 100)
	set = nodeSet(g)
	if len(set) != 3 || !set[ids[3]] || !set[ids[2]] {
		t.Fatalf("depth 2 from server: %v", set)
	}
}

func TestBuildGraphTruncates(t *testing.T) {
	nodes, edges, ids := chain()
	g := BuildGraph(nodes, edges, &ids[0], 4, 3)
	if !g.Truncated || len(g.Nodes) != 3 {
		t.Fatalf("expected truncation at 3 nodes: %d truncated=%v", len(g.Nodes), g.Truncated)
	}
	g = BuildGraph(nodes, edges, nil, 1, 2)
	if !g.Truncated || len(g.Nodes) != 2 {
		t.Fatalf("expected full-graph truncation: %d", len(g.Nodes))
	}
}

func TestBuildGraphIsDeterministic(t *testing.T) {
	nodes, edges, ids := chain()
	a := BuildGraph(nodes, edges, &ids[2], 3, 100)
	b := BuildGraph(nodes, edges, &ids[2], 3, 100)
	for i := range a.Nodes {
		if a.Nodes[i].Data.ID != b.Nodes[i].Data.ID {
			t.Fatal("node order must be stable")
		}
	}
}

func TestParseNodeID(t *testing.T) {
	valid := "asset:01a0fc08-fb28-7d11-b6a7-419822df652e"
	if id, ok := ParseNodeID(valid); !ok || string(id) != valid {
		t.Fatalf("ParseNodeID(%q) = %q, %v", valid, id, ok)
	}
	for _, s := range []string{"", "asset", "asset:", "server:01a0fc08-fb28-7d11-b6a7-419822df652e", "asset:not-a-uuid", "employee:1 OR 1=1"} {
		if _, ok := ParseNodeID(s); ok {
			t.Errorf("ParseNodeID(%q) accepted", s)
		}
	}
}

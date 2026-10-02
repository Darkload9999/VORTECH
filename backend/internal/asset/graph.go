package asset

import (
	"sort"
	"strings"

	"github.com/google/uuid"
)

// NodeID identifies a graph node as "<kind>:<uuid>" with kind employee,
// identity or asset. The same string is the Cytoscape element id.
type NodeID string

// MakeNodeID builds a NodeID.
func MakeNodeID(kind string, id uuid.UUID) NodeID { return NodeID(kind + ":" + id.String()) }

// ParseNodeID validates a client-supplied node reference.
func ParseNodeID(s string) (NodeID, bool) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok {
		return "", false
	}
	switch kind {
	case "employee", "identity", "asset":
	default:
		return "", false
	}
	id, err := uuid.Parse(rest)
	if err != nil {
		return "", false
	}
	return MakeNodeID(kind, id), true
}

// NodeData is the payload of a Cytoscape node element.
type NodeData struct {
	ID          NodeID `json:"id"`
	Kind        string `json:"kind"`
	Type        string `json:"type"`
	Label       string `json:"label"`
	Code        string `json:"code"`
	Hostname    string `json:"hostname,omitempty"`
	Criticality string `json:"criticality,omitempty"`
	JobTitle    string `json:"job_title,omitempty"`
	Department  string `json:"department,omitempty"`
	Privileged  bool   `json:"privileged,omitempty"`
	Status      string `json:"status,omitempty"`
}

// EdgeData is the payload of a Cytoscape edge element.
type EdgeData struct {
	ID     string `json:"id"`
	Source NodeID `json:"source"`
	Target NodeID `json:"target"`
	Label  string `json:"label"`
}

// Element wraps data the way Cytoscape.js expects ({ data: {...} }).
type Element[T any] struct {
	Data T `json:"data"`
}

// Graph is a Cytoscape.js-compatible element set: cy.add([...nodes, ...edges]).
type Graph struct {
	Nodes     []Element[NodeData] `json:"nodes"`
	Edges     []Element[EdgeData] `json:"edges"`
	Root      *NodeID             `json:"root"`
	Depth     int                 `json:"depth"`
	Truncated bool                `json:"truncated"`
}

// Edge is a directed relationship between two nodes.
type Edge struct {
	ID     string
	Type   string
	Source NodeID
	Target NodeID
}

// BuildGraph returns the subgraph within depth hops of root (edges are
// traversed in both directions), or the whole graph when root is nil.
// At most maxNodes nodes are returned; Truncated reports a cut.
func BuildGraph(nodes map[NodeID]NodeData, edges []Edge, root *NodeID, depth, maxNodes int) Graph {
	g := Graph{Root: root, Depth: depth, Nodes: []Element[NodeData]{}, Edges: []Element[EdgeData]{}}

	selected := map[NodeID]bool{}
	if root == nil {
		ids := make([]NodeID, 0, len(nodes))
		for id := range nodes {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			if len(selected) >= maxNodes {
				g.Truncated = true
				break
			}
			selected[id] = true
		}
	} else {
		adj := map[NodeID][]NodeID{}
		for _, e := range edges {
			adj[e.Source] = append(adj[e.Source], e.Target)
			adj[e.Target] = append(adj[e.Target], e.Source)
		}
		selected[*root] = true
		frontier := []NodeID{*root}
	bfs:
		for d := 0; d < depth && len(frontier) > 0; d++ {
			var next []NodeID
			for _, n := range frontier {
				neighbours := adj[n]
				sort.Slice(neighbours, func(i, j int) bool { return neighbours[i] < neighbours[j] })
				for _, m := range neighbours {
					if selected[m] {
						continue
					}
					if len(selected) >= maxNodes {
						g.Truncated = true
						break bfs
					}
					selected[m] = true
					next = append(next, m)
				}
			}
			frontier = next
		}
	}

	for id := range selected {
		if n, ok := nodes[id]; ok {
			g.Nodes = append(g.Nodes, Element[NodeData]{Data: n})
		}
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].Data.ID < g.Nodes[j].Data.ID })

	for _, e := range edges {
		if selected[e.Source] && selected[e.Target] {
			g.Edges = append(g.Edges, Element[EdgeData]{Data: EdgeData{ID: e.ID, Source: e.Source, Target: e.Target, Label: e.Type}})
		}
	}
	sort.Slice(g.Edges, func(i, j int) bool { return g.Edges[i].Data.ID < g.Edges[j].Data.ID })
	return g
}

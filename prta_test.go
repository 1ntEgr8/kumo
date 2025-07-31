package prta

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/callgraph"
	ogrta "golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

func countNodesAndEdges(cg *callgraph.Graph) (int, int, int) {
	if cg == nil {
		return 0, 0, 0
	}

	nodeCount := len(cg.Nodes)
	edgeSet := make(map[string]bool)
	totalEdges := 0

	for _, node := range cg.Nodes {
		totalEdges += len(node.Out)
		for _, edge := range node.Out {
			// Create unique key for this edge
			edgeKey := fmt.Sprintf("%s->%s", edge.Caller.Func.String(), edge.Callee.Func.String())
			edgeSet[edgeKey] = true
		}
	}

	uniqueEdges := len(edgeSet)
	return nodeCount, totalEdges, uniqueEdges
}

func compareCallGraphs(t *testing.T, og, pg *callgraph.Graph) {
	t.Log("Performing deep callgraph structure comparison...")

	if og == nil && pg == nil {
		t.Log("Both callgraphs are nil")
		return
	}

	require.NotNil(t, og, "Original callgraph should not be nil")
	require.NotNil(t, pg, "Parallel callgraph should not be nil")

	// Build hash sets for both graphs
	ogNodeHashes := make(map[uint64]bool)
	pgNodeHashes := make(map[uint64]bool)

	// Hash all nodes in both graphs
	for _, node := range og.Nodes {
		ogNodeHashes[NodeHash(node)] = true
	}

	for _, node := range pg.Nodes {
		pgNodeHashes[NodeHash(node)] = true
	}

	// Direct map comparison
	require.Equal(t, ogNodeHashes, pgNodeHashes, "Callgraph node structures should be identical")

	t.Logf("Callgraphs are structurally equivalent (%d unique nodes)", len(ogNodeHashes))
}

func TestPrta(t *testing.T) {
	ssaProg := nil

	entry := []*ssa.Function{}
	for f, ok := range ssautil.AllFunctions(ssaProg) {
		// Skip functions with receivers since they are not
		// valid program roots.
		if !ok || (f.Signature != nil && f.Signature.Recv() != nil) {
			continue
		}
		if f.Name() == "main" || f.Name() == "init" {
			entry = append(entry, f)
		}
	}

	// Run parallel RTA
	t.Log("Running parallel RTA...")
	start := time.Now()
	g := Analyze(entry, allPkgs, true, 1)
	prtaDuration := time.Since(start)
	require.NotNil(t, g, "nil callgraph returned by parallel RTA")

	// Count reachable functions
	reachableCount := 0
	g.Reachable.Range(func(key, value any) bool {
		reachableCount++
		return true
	})

	prtaNodes, prtaTotalEdges, prtaUniqueEdges := countNodesAndEdges(g.CallGraph)
	t.Logf("Parallel RTA: %d nodes, %d total edges, %d unique edges in %v",
		prtaNodes, prtaTotalEdges, prtaUniqueEdges, prtaDuration)
	t.Logf("Total reachable functions: %d, Call graph nodes: %d", reachableCount, prtaNodes)

	// Run original RTA for comparison
	t.Log("Running original RTA...")
	start = time.Now()
	og := ogrta.Analyze(entry, true)
	ogDuration := time.Since(start)
	require.NotNil(t, og, "nil callgraph returned by original RTA")

	ogNodes, ogTotalEdges, ogUniqueEdges := countNodesAndEdges(og.CallGraph)
	t.Logf("Original RTA: %d nodes, %d total edges, %d unique edges in %v",
		ogNodes, ogTotalEdges, ogUniqueEdges, ogDuration)

	// Performance comparison
	speedup := float64(ogDuration) / float64(prtaDuration)
	t.Logf("Performance: %.2fx speedup (original: %v, parallel: %v)", speedup, ogDuration, prtaDuration)

	// Verify results are equivalent
	t.Log("Comparing results...")
	require.Equal(t, ogNodes, prtaNodes, "Node count should be equal")
	require.Equal(t, ogUniqueEdges, prtaUniqueEdges, "Unique edge count should be equal")

	// Deep structural comparison using node hashes
	compareCallGraphs(t, og.CallGraph, g.CallGraph)

	t.Logf("Results are equivalent: %d nodes, %d unique edges", ogNodes, ogUniqueEdges)
}

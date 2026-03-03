package kumo_test

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/1ntEgr8/kumo"
	"github.com/1ntEgr8/kumo/implementations/prta_kumo"
	"github.com/1ntEgr8/kumo/implementations/prta_kumo_nonblocking"
	"github.com/1ntEgr8/kumo/implementations/prta_naive"
	"github.com/1ntEgr8/kumo/implementations/prta_nonblocking"
	"github.com/1ntEgr8/kumo/implementations/srta"
	"github.com/1ntEgr8/kumo/implementations/srta_kumo"
	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// callgraphMaxDepth computes the maximum depth (shortest path to furthest node) from the root
// using BFS. This is O(V+E) and gives a consistent, well-defined result.
// Returns the max depth and the function at that depth.
func callgraphMaxDepth(cg *callgraph.Graph) (int, string) {
	if cg == nil || cg.Root == nil {
		return 0, ""
	}

	type queueItem struct {
		node  *callgraph.Node
		depth int
	}

	visited := make(map[*callgraph.Node]bool)
	queue := []queueItem{{node: cg.Root, depth: 0}}
	visited[cg.Root] = true

	maxDepth := 0
	var furthestFunc string

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		if item.depth > maxDepth {
			maxDepth = item.depth
			if item.node.Func != nil {
				furthestFunc = item.node.Func.String()
			}
		}

		// Deduplicate callees
		seenCallees := make(map[*callgraph.Node]bool)
		for _, edge := range item.node.Out {
			callee := edge.Callee
			if callee == nil || visited[callee] || seenCallees[callee] {
				continue
			}
			seenCallees[callee] = true
			visited[callee] = true
			queue = append(queue, queueItem{node: callee, depth: item.depth + 1})
		}
	}

	return maxDepth, furthestFunc
}

// countUniqueEdges counts edges after deduplicating by (caller, callee) pair.
func countUniqueEdges(cg *callgraph.Graph) int {
	if cg == nil {
		return 0
	}
	type edgeKey struct {
		caller, callee *ssa.Function
	}
	seen := make(map[edgeKey]bool)
	for _, node := range cg.Nodes {
		for _, edge := range node.Out {
			key := edgeKey{caller: edge.Caller.Func, callee: edge.Callee.Func}
			seen[key] = true
		}
	}
	return len(seen)
}

var k8sPath = flag.String("k8s-path", "", "Path to kubernetes repository (v1.35.1)")

func TestImplementations(t *testing.T) {
	// Initialize all implementations to verify they compile and can be instantiated

	// Parallel implementations
	_ = prta_naive.New()
	_ = prta_kumo.New()
	_ = prta_nonblocking.New()
	_ = prta_kumo_nonblocking.New()

	// Sequential implementations
	_ = srta.New()
	_ = srta_kumo.New()

	// TODO: Add actual test logic
}

// TestRTAVariantsKubelet runs all RTA variants (parallel and sequential) on the
// Kubernetes kubelet main function and times each variant.
//
// To run this test, you need a compatible kubernetes version available:
//
//	git clone --depth 1 --branch v1.32.0 https://github.com/kubernetes/kubernetes.git /path/to/kubernetes
//
// Run with:
//
//	go test -v -run TestRTAVariantsKubelet -timeout 30m -k8s-path=/path/to/kubernetes
//
// Or set the K8S_PATH environment variable:
//
//	K8S_PATH=/path/to/kubernetes go test -v -run TestRTAVariantsKubelet -timeout 30m
func TestRTAVariantsKubelet(t *testing.T) {
	// Get kubernetes repo path from flag or environment variable
	repoPath := *k8sPath
	if repoPath == "" {
		repoPath = os.Getenv("K8S_PATH")
	}
	if repoPath == "" {
		t.Skip("Skipping: kubernetes repo path not provided. Use -k8s-path flag or K8S_PATH env var")
	}

	// Verify the path exists
	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		t.Fatalf("Kubernetes repo path does not exist: %s", repoPath)
	}

	// Load the kubelet package from the local kubernetes repo
	// This corresponds to: https://github.com/kubernetes/kubernetes/blob/v1.35.1/cmd/kubelet/kubelet.go
	kubeletPkg := "./cmd/kubelet"

	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedImports |
			packages.NeedDeps |
			packages.NeedTypes |
			packages.NeedSyntax |
			packages.NeedTypesInfo |
			packages.NeedTypesSizes,
		Dir: repoPath,
	}

	t.Logf("Loading package %s from %s...", kubeletPkg, repoPath)
	loadStart := time.Now()
	pkgs, err := packages.Load(cfg, kubeletPkg)
	if err != nil {
		t.Fatalf("Failed to load package: %v", err)
	}
	t.Logf("Package loading took %v", time.Since(loadStart))

	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("Errors loading packages")
	}

	// Build SSA program
	t.Log("Building SSA program...")
	ssaStart := time.Now()
	prog, _ := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	t.Logf("SSA building took %v", time.Since(ssaStart))

	// Find the main function
	var mainFn *ssa.Function
	for _, pkg := range prog.AllPackages() {
		if pkg.Pkg.Name() == "main" {
			if fn := pkg.Func("main"); fn != nil {
				mainFn = fn
				break
			}
		}
	}

	if mainFn == nil {
		t.Fatal("Could not find main function")
	}

	t.Logf("Found main function: %s", mainFn.String())

	// Collect all init functions as roots
	roots := []*ssa.Function{mainFn}
	for _, pkg := range prog.AllPackages() {
		if initFn := pkg.Func("init"); initFn != nil {
			roots = append(roots, initFn)
		}
	}
	t.Logf("Total roots (main + init functions): %d", len(roots))

	// Number of workers for parallel implementations
	numWorkers := runtime.NumCPU()
	t.Logf("Using %d workers for parallel implementations", numWorkers)

	// Define all RTA variants to test (parallel and sequential)
	type rtaVariant struct {
		name string
		impl kumo.RTA
	}

	variants := []rtaVariant{
		// Parallel implementations
		{"prta_naive", prta_naive.New()},
		{"prta_kumo", prta_kumo.New()},
		{"prta_nonblocking", prta_nonblocking.New()},
		{"prta_kumo_nonblocking", prta_kumo_nonblocking.New()},
		// Sequential implementations
		{"srta", srta.New()},
		{"srta_kumo", srta_kumo.New()},
	}

	// Results summary
	type result struct {
		name            string
		duration        time.Duration
		reachableCount  int
		nodeCount       int
		edgeCount       int
		uniqueEdgeCount int
		span            int
	}
	var results []result

	// Run each variant
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			t.Logf("Running %s...", v.name)

			// Force GC before each run for more consistent timing
			runtime.GC()

			start := time.Now()
			res := v.impl.Analyze(roots, true, numWorkers)
			duration := time.Since(start)

			if res == nil {
				t.Fatalf("%s returned nil result", v.name)
			}

			reachable := res.GetReachable()
			cg := res.GetCallGraph()

			var nodeCount, edgeCount, uniqueEdgeCount, span int
			if cg != nil {
				nodeCount = len(cg.Nodes)
				for _, node := range cg.Nodes {
					edgeCount += len(node.Out)
				}
				uniqueEdgeCount = countUniqueEdges(cg)
				span, _ = callgraphMaxDepth(cg)
			}

			t.Logf("%s completed in %v", v.name, duration)
			t.Logf("  Reachable functions: %d", len(reachable))
			t.Logf("  Call graph nodes: %d, edges: %d (unique: %d), max depth: %d", nodeCount, edgeCount, uniqueEdgeCount, span)

			results = append(results, result{
				name:            v.name,
				duration:        duration,
				reachableCount:  len(reachable),
				nodeCount:       nodeCount,
				edgeCount:       edgeCount,
				uniqueEdgeCount: uniqueEdgeCount,
				span:            span,
			})
		})
	}

	// Print summary
	t.Log("\n=== SUMMARY ===")
	fmt.Printf("\n%-25s %15s %15s %15s %15s %15s %10s %12s\n", "Variant", "Duration", "Reachable", "CG Nodes", "CG Edges", "Unique Edges", "Depth", "Nodes/Depth")
	fmt.Printf("%s\n", "------------------------------------------------------------------------------------------------------------------------------------------")
	for _, r := range results {
		var ratio float64
		if r.span > 0 {
			ratio = float64(r.nodeCount) / float64(r.span)
		}
		fmt.Printf("%-25s %15v %15d %15d %15d %15d %10d %12.2f\n", r.name, r.duration, r.reachableCount, r.nodeCount, r.edgeCount, r.uniqueEdgeCount, r.span, ratio)
	}
}

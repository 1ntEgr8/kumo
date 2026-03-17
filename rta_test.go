package kumo_test

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

// getNumWorkers returns the number of workers to use for parallel implementations.
// It reads from NUM_WORKERS env var, defaulting to runtime.NumCPU().
func getNumWorkers() int {
	if s := os.Getenv("NUM_WORKERS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return runtime.NumCPU()
}

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

// benchmarkTarget describes an OSS project to benchmark
type benchmarkTarget struct {
	Name    string // Display name (used for -benchmark flag)
	DirName string // Directory name in datasets folder
	PkgPath string // Package path relative to repo root
}

// ossBenchmarkTargets defines the OSS projects available for benchmarking
var ossBenchmarkTargets = []benchmarkTarget{
	{"kubernetes-kubelet", "kubernetes", "./cmd/kubelet"},
	{"kubernetes-apiserver", "kubernetes", "./cmd/kube-apiserver"},
	{"prometheus", "prometheus", "./cmd/prometheus"},
	{"etcd", "etcd/server", "."},
	{"terraform", "terraform", "."},
	{"hugo", "hugo", "."},
	{"consul", "consul", "."},
	{"vault", "vault", "."},
{"traefik", "traefik", "./cmd/traefik"},
	{"minio", "minio", "."},
	{"gitea", "gitea", "."},
	{"containerd", "containerd", "./cmd/containerd"},
	{"nats-server", "nats-server", "."},
	{"caddy", "caddy", "./cmd/caddy"},
}

// findTarget looks up a benchmark target by name
func findTarget(name string) *benchmarkTarget {
	for i := range ossBenchmarkTargets {
		if ossBenchmarkTargets[i].Name == name {
			return &ossBenchmarkTargets[i]
		}
	}
	return nil
}

// listTargets returns a comma-separated list of available target names
func listTargets() string {
	names := make([]string, len(ossBenchmarkTargets))
	for i, t := range ossBenchmarkTargets {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

// rtaVariant describes an RTA implementation variant
type rtaVariant struct {
	name string
	impl kumo.RTA
}

// rtaResult holds the benchmark results for one variant
type rtaResult struct {
	name            string
	duration        time.Duration
	reachableCount  int
	nodeCount       int
	edgeCount       int
	uniqueEdgeCount int
	depth           int
}

// allRTAVariants contains all available RTA implementations
var allRTAVariants = map[string]func() kumo.RTA{
	"prta_naive":            func() kumo.RTA { return prta_naive.New() },
	"prta_kumo":             func() kumo.RTA { return prta_kumo.New() },
	"prta_nonblocking":      func() kumo.RTA { return prta_nonblocking.New() },
	"prta_kumo_nonblocking": func() kumo.RTA { return prta_kumo_nonblocking.New() },
	"srta":                  func() kumo.RTA { return srta.New() },
	"srta_kumo":             func() kumo.RTA { return srta_kumo.New() },
}

// defaultVariantOrder defines the default order of variants
var defaultVariantOrder = []string{
	"prta_naive", "prta_kumo", "prta_nonblocking", "prta_kumo_nonblocking", "srta", "srta_kumo",
}

// getRTAVariants returns RTA variants based on VARIANTS env var
// VARIANTS can be comma-separated list like "prta_naive,srta" or "all" (default)
func getRTAVariants() []rtaVariant {
	variantFilter := os.Getenv("VARIANTS")
	if variantFilter == "" || variantFilter == "all" {
		// Return all in default order
		variants := make([]rtaVariant, 0, len(defaultVariantOrder))
		for _, name := range defaultVariantOrder {
			variants = append(variants, rtaVariant{name, allRTAVariants[name]()})
		}
		return variants
	}

	// Parse comma-separated list
	requested := strings.Split(variantFilter, ",")
	variants := make([]rtaVariant, 0, len(requested))
	for _, name := range requested {
		name = strings.TrimSpace(name)
		if factory, ok := allRTAVariants[name]; ok {
			variants = append(variants, rtaVariant{name, factory()})
		}
	}
	return variants
}

// listVariants returns a comma-separated list of available variant names
func listVariants() string {
	return strings.Join(defaultVariantOrder, ", ")
}

// loadSSAProgram loads and builds an SSA program from the given directory and package path
func loadSSAProgram(t *testing.T, repoPath, pkgPath string) (*ssa.Program, *ssa.Function, []*ssa.Function) {
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

	t.Logf("Loading package %s from %s...", pkgPath, repoPath)
	loadStart := time.Now()
	pkgs, err := packages.Load(cfg, pkgPath)
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
		if pkg != nil && pkg.Pkg.Name() == "main" {
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
		if pkg != nil {
			if initFn := pkg.Func("init"); initFn != nil {
				roots = append(roots, initFn)
			}
		}
	}
	t.Logf("Total roots (main + init functions): %d", len(roots))

	return prog, mainFn, roots
}

// runRTABenchmark runs RTA variants on the given roots and returns results
func runRTABenchmark(t *testing.T, roots []*ssa.Function, numWorkers int) []rtaResult {
	variants := getRTAVariants()
	var results []rtaResult

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

			var nodeCount, edgeCount, uniqueEdgeCount, depth int
			if cg != nil {
				nodeCount = len(cg.Nodes)
				for _, node := range cg.Nodes {
					edgeCount += len(node.Out)
				}
				uniqueEdgeCount = countUniqueEdges(cg)
				depth, _ = callgraphMaxDepth(cg)
			}

			t.Logf("%s completed in %v", v.name, duration)
			t.Logf("  Reachable functions: %d", len(reachable))
			t.Logf("  Call graph nodes: %d, edges: %d (unique: %d), max depth: %d", nodeCount, edgeCount, uniqueEdgeCount, depth)

			results = append(results, rtaResult{
				name:            v.name,
				duration:        duration,
				reachableCount:  len(reachable),
				nodeCount:       nodeCount,
				edgeCount:       edgeCount,
				uniqueEdgeCount: uniqueEdgeCount,
				depth:           depth,
			})
		})
	}

	return results
}

// printBenchmarkSummary prints a summary table of benchmark results
func printBenchmarkSummary(t *testing.T, targetName string, results []rtaResult) {
	t.Logf("\n=== SUMMARY: %s ===", targetName)
	fmt.Printf("\n%-25s %15s %15s %15s %15s %15s %10s %12s\n", "Variant", "Duration", "Reachable", "CG Nodes", "CG Edges", "Unique Edges", "Depth", "Nodes/Depth")
	fmt.Printf("%s\n", "------------------------------------------------------------------------------------------------------------------------------------------")
	for _, r := range results {
		var ratio float64
		if r.depth > 0 {
			ratio = float64(r.nodeCount) / float64(r.depth)
		}
		fmt.Printf("%-25s %15v %15d %15d %15d %15d %10d %12.2f\n", r.name, r.duration, r.reachableCount, r.nodeCount, r.edgeCount, r.uniqueEdgeCount, r.depth, ratio)
	}
}

// TestRTABenchmark runs RTA benchmarks on OSS projects.
//
// Environment variables:
//
//	BENCHMARK      - Required. Target name (e.g., "etcd", "consul", "kubernetes-kubelet")
//	DATASETS_ROOT  - Optional. Path to datasets directory (default: "./datasets")
//	NUM_WORKERS    - Optional. Number of parallel workers (default: runtime.NumCPU())
//	VARIANTS       - Optional. Comma-separated list of variants to run (default: "all")
//	               Available: prta_naive, prta_kumo, prta_nonblocking, prta_kumo_nonblocking, srta, srta_kumo
//	CSV_OUTPUT     - Optional. Path to a CSV file to write results to
//
// Setup:
//
//	./scripts/clone_datasets.sh ./datasets
//
// Examples:
//
//	# Run etcd benchmark with all variants
//	BENCHMARK=etcd go test -v -run TestRTABenchmark -timeout 30m
//
//	# Run consul with specific variants and 4 workers
//	BENCHMARK=consul VARIANTS=prta_kumo,srta NUM_WORKERS=4 go test -v -run TestRTABenchmark -timeout 30m
//
//	# Run kubernetes-kubelet with custom datasets path
//	BENCHMARK=kubernetes-kubelet DATASETS_ROOT=/path/to/datasets go test -v -run TestRTABenchmark -timeout 30m
//
// Available targets:
//
//	kubernetes-kubelet, kubernetes-apiserver, prometheus, etcd, terraform, hugo,
//	consul, vault, grafana, traefik, minio, gitea, containerd, nats-server, caddy, syncthing
func TestRTABenchmark(t *testing.T) {
	datasetsRoot := os.Getenv("DATASETS_ROOT")
	if datasetsRoot == "" {
		datasetsRoot = "./datasets"
	}

	// Determine targets: all (empty), or comma-separated list
	benchmarkName := os.Getenv("BENCHMARK")
	var targets []benchmarkTarget
	if benchmarkName == "" {
		targets = ossBenchmarkTargets
	} else {
		for _, name := range strings.Split(benchmarkName, ",") {
			name = strings.TrimSpace(name)
			target := findTarget(name)
			if target == nil {
				t.Fatalf("Unknown benchmark target: %s\nAvailable targets: %s", name, listTargets())
			}
			targets = append(targets, *target)
		}
	}

	numWorkers := getNumWorkers()
	csvPath := os.Getenv("CSV_OUTPUT")

	for _, target := range targets {
		t.Run(target.Name, func(t *testing.T) {
			repoPath := filepath.Join(datasetsRoot, target.DirName)
			if _, err := os.Stat(repoPath); os.IsNotExist(err) {
				t.Skipf("Dataset not found: %s\nRun: ./scripts/clone_datasets.sh %s", repoPath, datasetsRoot)
			}

			variants := getRTAVariants()

			t.Logf("=== Benchmarking %s ===", target.Name)
			t.Logf("Dataset path: %s", repoPath)
			t.Logf("Workers: %d", numWorkers)
			t.Logf("Variants: %s", variantNames(variants))

			_, _, roots := loadSSAProgram(t, repoPath, target.PkgPath)
			results := runRTABenchmark(t, roots, numWorkers)
			printBenchmarkSummary(t, target.Name, results)

			if csvPath != "" {
				writeResultsCSV(t, csvPath, target.Name, numWorkers, results)
			}
		})
	}
}

// writeResultsCSV writes benchmark results to a CSV file.
// If the file already exists, it appends rows (no duplicate header).
// If the file is new, it writes the header first.
func writeResultsCSV(t *testing.T, path, targetName string, numWorkers int, results []rtaResult) {
	t.Helper()

	header := []string{"target", "variant", "num_workers", "duration_ms", "reachable", "cg_nodes", "cg_edges", "unique_edges", "depth"}

	// Check if file already exists (to decide whether to write header)
	writeHeader := true
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		writeHeader = false
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("Failed to open CSV file %s: %v", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	if writeHeader {
		if err := w.Write(header); err != nil {
			t.Fatalf("Failed to write CSV header: %v", err)
		}
	}

	workers := strconv.Itoa(numWorkers)
	for _, r := range results {
		row := []string{
			targetName,
			r.name,
			workers,
			fmt.Sprintf("%.2f", float64(r.duration.Milliseconds())),
			strconv.Itoa(r.reachableCount),
			strconv.Itoa(r.nodeCount),
			strconv.Itoa(r.edgeCount),
			strconv.Itoa(r.uniqueEdgeCount),
			strconv.Itoa(r.depth),
		}
		if err := w.Write(row); err != nil {
			t.Fatalf("Failed to write CSV row: %v", err)
		}
	}

	t.Logf("Results written to %s", path)
}

// variantNames returns a comma-separated list of variant names
func variantNames(variants []rtaVariant) string {
	names := make([]string, len(variants))
	for i, v := range variants {
		names[i] = v.name
	}
	return strings.Join(names, ", ")
}

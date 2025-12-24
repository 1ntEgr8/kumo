package kumo

import (
	"golang.org/x/tools/go/callgraph"
	rtapkg "golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
)

// Result interface defines methods to access RTA analysis results.
// Different implementations can use different internal representations
// (e.g., sync.Map for parallel, regular map for sequential) and convert
// on demand via Materialize().
type Result interface {
	// GetCallGraph returns the discovered callgraph.
	// It does not include edges for calls made via reflection.
	GetCallGraph() *callgraph.Graph

	// GetReachable returns the set of reachable functions and methods.
	// This includes exported methods of runtime types, since
	// they may be accessed via reflection.
	// The value indicates whether the function is address-taken.
	GetReachable() map[*ssa.Function]struct{ AddrTaken bool }

	// GetRuntimeTypes returns the set of types that are needed at
	// runtime, for interfaces or reflection.
	//
	// The value indicates whether the type is inaccessible to reflection.
	// Consider:
	//     type A struct{B}
	//     fmt.Println(new(A))
	// Types *A, A and B are accessible to reflection, but the unnamed
	// type struct{B} is not.
	GetRuntimeTypes() typeutil.Map

	// Materialize converts the result to the standard golang.org/x/tools/go/callgraph/rta.Result.
	// This allows implementations to use optimal internal representations
	// (e.g., sync.Map for parallel implementations) and convert on demand.
	Materialize() *rtapkg.Result
}

// RTA defines the interface for Rapid Type Analysis implementations.
type RTA interface {
	// Analyze performs Rapid Type Analysis starting at the specified root functions.
	// It returns nil if no roots were specified.
	//
	// The root functions must be one or more entrypoints (main and init functions)
	// of a complete SSA program, with function bodies for all dependencies,
	// constructed with the [ssa.InstantiateGenerics] mode flag.
	//
	// If buildCallGraph is true, Result.CallGraph will contain a call graph;
	// otherwise, only the other fields (reachable functions) are populated.
	Analyze(roots []*ssa.Function, buildCallGraph bool, numWorkers int) Result
}

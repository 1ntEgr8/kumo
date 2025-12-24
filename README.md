# Kumo

A collection of Rapid Type Analysis (RTA) implementations for Go programs, optimized for performance through parallelization and algorithmic improvements.

## Overview

Kumo provides multiple RTA implementations with different optimization strategies:

### Parallel Implementations
- **prta_naive** - Basic parallel RTA with work-stealing queue
- **prta_kumo** - Parallel RTA with method-based indexing optimization
- **prta_nonblocking** - Parallel RTA with heavily contended locks replaced
- **prta_kumo_nonblocking** - Combines method indexing and non blocking

### Sequential Implementations
- **srta** - Sequential baseline RTA implementation
- **srta_kumo** - Sequential RTA with method-based indexing

## Installation

```bash
go get github.com/1ntEgr8/kumo
```

## Usage

```go
import (
    "github.com/1ntEgr8/kumo"
    "github.com/1ntEgr8/kumo/implementations/prta_naive"
)

// Using the naive parallel implementation
analyzer := prta_naive.New()
result := analyzer.Analyze(roots, buildCallGraph, numWorkers)

// All implementations follow the same pattern:
// - prta_naive.New()
// - prta_kumo.New()
// - prta_nonblocking.New()
// - prta_kumo_nonblocking.New()
// - srta.New()
// - srta_kumo.New()

// Access results through the interface methods:
callGraph := result.GetCallGraph()
reachable := result.GetReachable()
runtimeTypes := result.GetRuntimeTypes()

// Or convert to standard golang.org/x/tools/go/callgraph/rta.Result:
standardResult := result.Materialize()
```

## Architecture

All implementations conform to the `kumo.RTA` interface:

```go
type RTA interface {
    Analyze(roots []*ssa.Function, buildCallGraph bool, numWorkers int) Result
}
```

The `Result` interface provides:
```go
type Result interface {
    GetCallGraph() *callgraph.Graph
    GetReachable() map[*ssa.Function]struct{AddrTaken bool}
    GetRuntimeTypes() typeutil.Map
    Materialize() *rtapkg.Result
}
```

### Result Interface Design

Each implementation uses optimal internal data structures:
- **Sequential implementations** (`srta`, `srta_kumo`): Use `typeutil.Map` directly for minimal overhead
- **Parallel implementations** (`prta_*`): Use `sync.Map` and `utils.TypeMap` for thread-safe concurrent access

The `Materialize()` method converts to the standard `golang.org/x/tools/go/callgraph/rta.Result` type for compatibility with existing tools

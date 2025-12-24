// Parallel RTA
//
// Code is heavily borrowed from Go's RTA package. We have modified it make
// it parallel.

package prta_naive

import (
	"fmt"
	"go/types"
	"hash/crc32"
	"runtime"
	"sync"
	"unsafe"

	"github.com/1ntEgr8/kumo"
	"github.com/1ntEgr8/kumo/lockfree"
	"github.com/1ntEgr8/kumo/utils"
	"golang.org/x/tools/go/callgraph"
	rtapkg "golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"
)

// Result implements the kumo.Result interface for parallel RTA.
// It uses sync.Map internally for concurrent access during analysis.
type Result struct {
	callGraph    *callgraph.Graph
	reachable    sync.Map // map[*ssa.Function]bool (AddrTaken)
	runtimeTypes utils.TypeMap
}

// GetCallGraph returns the discovered callgraph.
func (r *Result) GetCallGraph() *callgraph.Graph {
	return r.callGraph
}

// GetReachable returns the set of reachable functions.
// This converts from the internal sync.Map to a regular map.
func (r *Result) GetReachable() map[*ssa.Function]struct{ AddrTaken bool } {
	reachableMap := make(map[*ssa.Function]struct{ AddrTaken bool })
	r.reachable.Range(func(key, value any) bool {
		reachableMap[key.(*ssa.Function)] = struct{ AddrTaken bool }{AddrTaken: value.(bool)}
		return true
	})
	return reachableMap
}

// GetRuntimeTypes returns the set of runtime types.
func (r *Result) GetRuntimeTypes() typeutil.Map {
	// Convert utils.TypeMap to typeutil.Map
	var tm typeutil.Map
	tm.SetHasher(typeutil.MakeHasher())
	r.runtimeTypes.Iterate(func(key types.Type, value any) {
		tm.Set(key, value)
	})
	return tm
}

// Materialize converts the result to the standard rta.Result.
// For parallel implementation, this performs the conversion from sync.Map to regular map.
func (r *Result) Materialize() *rtapkg.Result {
	return &rtapkg.Result{
		CallGraph:    r.GetCallGraph(),
		Reachable:    r.GetReachable(),
		RuntimeTypes: r.GetRuntimeTypes(),
	}
}

const _dedupEdgesSmallThreshold = 10

type workItem interface {
	doWork(r *rta)
}

type processFunctionWorkItem struct {
	fn *ssa.Function
}

func (w *processFunctionWorkItem) doWork(r *rta) {
	r.visitFunc(w.fn)
}

type deduplicateEdgesWorkItem struct {
	nodes     []*callgraph.Node
	semaphore chan []*callgraph.Node
}

func (w *deduplicateEdgesWorkItem) doWork(r *rta) {
	for _, node := range w.nodes {
		if len(node.In) > 1 {
			node.In = dedupEdges(node.In)
		}
		if len(node.Out) > 1 {
			node.Out = dedupEdges(node.Out)
		}
	}
	// Return slice to semaphore for recycling
	w.semaphore <- w.nodes[:0]
}

// Working state of the RTA algorithm.
type rta struct {
	result *Result

	prog *ssa.Program

	reflectValueCall *ssa.Function // (*reflect.Value).Call, iff part of prog

	work     *lockfree.MSQueue[workItem]
	workDone chan struct{}
	workWg   sync.WaitGroup

	// addrTakenFuncsBySig contains all address-taken *Functions, grouped by signature.
	// Keys are *types.Signature, values are map[*ssa.Function]bool sets.
	addrTakenFuncsBySig utils.TypeMap

	// dynCallSites contains all dynamic "call"-mode call sites, grouped by signature.
	// Keys are *types.Signature, values are unordered ssa.CallInstruction sets (represented using sync.Map).
	dynCallSites utils.TypeMap

	// invokeSites contains all "invoke"-mode call sites, grouped by interface.
	// Keys are *types.Interface (never *types.Named),
	// Values are unordered ssa.CallInstruction sets (represented using sync.Map).
	invokeSites utils.TypeMap

	// The following two maps together define the subset of the
	// m:n "implements" relation needed by the algorithm.

	// concreteTypes maps each concrete type to information about it.
	// Keys are types.Type, values are *concreteTypeInfo.
	// Only concrete types used as MakeInterface operands are included.
	concreteTypes utils.TypeMap

	// interfaceTypes maps each interface type to information about it.
	// Keys are *types.Interface, values are *interfaceTypeInfo.
	// Only interfaces used in "invoke"-mode CallInstructions are included.
	interfaceTypes utils.TypeMap

	callgraphLk sync.Mutex
}

type concreteTypeInfo struct {
	C      types.Type
	mset   *types.MethodSet
	fprint uint64 // fingerprint of method set

	implements utils.TypeMap

	// We use a channel to signal when initialization is done because `concreteTypeInfo` is made visible in the `concreteTypes` map before we populate the `implements` field. This allows consumers that only wish to know the presence of the `concreteTypeInfo` to progress, while those that need to access `implements` must block on the `initDone` channel.
	initDone chan struct{} // closed when initialization is complete
}

type interfaceTypeInfo struct {
	I      *types.Interface
	mset   *types.MethodSet
	fprint uint64

	implementations utils.TypeMap

	// See comment in `concreteTypeInfo`
	initDone chan struct{} // closed when initialization is complete
}

type callSites struct {
	sites sync.Map
}

// addReachable marks a function as potentially callable at run-time,
// and ensures that it gets processed.
func (r *rta) addReachable(f *ssa.Function, addrTaken bool) {
	existing, loaded := r.result.reachable.LoadOrStore(f, addrTaken)
	if loaded && addrTaken && !existing.(bool) {
		// Need to update existing entry to set addr taken to true
		r.result.reachable.Store(f, true)
	}
	if !loaded {
		// First time seeing f.  Add it to the worklist.
		r.addFunctionToWorklist(f)
	}
}

func (r *rta) addToWorklist(item workItem) {
	// In the current implementation, new work items are only added while processing an existing work item. The worker calls workWg.Add(1) for each new work item it adds and only then does it call workWg.Done(), after it has done processing _it's own work item_. This ensures that the main thread does not exit from the wait group while there is still work to do.
	r.workWg.Add(1)
	r.work.Enqueue(item)
}

func (r *rta) addFunctionToWorklist(f *ssa.Function) {
	r.addToWorklist(&processFunctionWorkItem{fn: f})
}

// addEdge adds the specified call graph edge, and marks it reachable.
// addrTaken indicates whether to mark the callee as "address-taken".
// site is nil for calls made via reflection.
func (r *rta) addEdge(caller *ssa.Function, site ssa.CallInstruction, callee *ssa.Function, addrTaken bool) {
	r.addReachable(callee, addrTaken)

	if g := r.result.callGraph; g != nil {
		if caller == nil {
			panic(site)
		}

		// TODO(elton): Remove this lock after callgraph type is made thread-safe
		r.callgraphLk.Lock()

		// g.CreateNode takes care of node de-duplication
		from := g.CreateNode(caller)
		to := g.CreateNode(callee)
		callgraph.AddEdge(from, site, to)

		r.callgraphLk.Unlock()
	}
}

// ---------- addrTakenFuncs × dynCallSites ----------

// visitAddrTakenFunc is called each time we encounter an address-taken function f.
func (r *rta) visitAddrTakenFunc(f *ssa.Function) {
	// Create two-level map (Signature -> Function -> bool).
	S := f.Signature

	val, _ := r.addrTakenFuncsBySig.LoadOrStore(S, &sync.Map{})
	funcs := val.(*sync.Map)

	if _, loaded := funcs.LoadOrStore(f, true); !loaded {
		// First time seeing f.

		// If we've seen any dyncalls of this type, mark it reachable,
		// and add call graph edges.
		val, _ := r.dynCallSites.LoadOrStore(S, &callSites{})
		s := val.(*callSites)
		s.sites.Range(func(key any, value any) bool {
			site := key.(ssa.CallInstruction)
			r.addEdge(site.Parent(), site, f, true)
			return true
		})

		// If the program includes (*reflect.Value).Call,
		// add a dynamic call edge from it to any address-taken
		// function, regardless of signature.
		//
		// This isn't perfect.
		// - The actual call comes from an internal function
		//   called reflect.call, but we can't rely on that here.
		// - reflect.Value.CallSlice behaves similarly,
		//   but we don't bother to create callgraph edges from
		//   it as well as it wouldn't fundamentally change the
		//   reachability but it would add a bunch more edges.
		// - We assume that if reflect.Value.Call is among
		//   the dependencies of the application, it is itself
		//   reachable. (It would be more accurate to defer
		//   all the addEdges below until r.V.Call itself
		//   becomes reachable.)
		// - Fake call graph edges are added from r.V.Call to
		//   each address-taken function, but not to every
		//   method reachable through a materialized rtype,
		//   which is a little inconsistent. Still, the
		//   reachable set includes both kinds, which is what
		//   matters for e.g. deadcode detection.)
		if r.reflectValueCall != nil {
			var site ssa.CallInstruction = nil // can't find actual call site
			r.addEdge(r.reflectValueCall, site, f, true)
		}
	}
}

// visitDynCall is called each time we encounter a dynamic "call"-mode call.
func (r *rta) visitDynCall(site ssa.CallInstruction) {
	S := site.Common().Signature()

	// Record the call site.
	// Note: A function is never added more than once to the worklist. A callsite is added only by visiting each instruction in the function. So, it can also be added to s.sites only once.
	val, _ := r.dynCallSites.LoadOrStore(S, &callSites{})
	s := val.(*callSites)
	s.sites.Store(site, struct{}{})

	// For each function of signature S that we know is address-taken,
	// add an edge and mark it reachable.
	val, _ = r.addrTakenFuncsBySig.LoadOrStore(S, &sync.Map{})
	funcs := val.(*sync.Map)
	funcs.Range(func(key any, value any) bool {
		g := key.(*ssa.Function)
		r.addEdge(site.Parent(), site, g, true)
		return true
	})
}

// ---------- concrete types × invoke sites ----------

// addInvokeEdge is called for each new pair (site, C) in the matrix.
func (r *rta) addInvokeEdge(site ssa.CallInstruction, C types.Type) {
	// Ascertain the concrete method of C to be called.
	imethod := site.Common().Method
	cmethod := r.prog.LookupMethod(C, imethod.Pkg(), imethod.Name())
	r.addEdge(site.Parent(), site, cmethod, true)
}

// visitInvoke is called each time the algorithm encounters an "invoke"-mode call.
func (r *rta) visitInvoke(site ssa.CallInstruction) {
	I := site.Common().Value.Type().Underlying().(*types.Interface)

	// Record the invoke site.
	val, _ := r.invokeSites.LoadOrStore(I, &callSites{})
	s := val.(*callSites)

	s.sites.Store(site, struct{}{})

	// Add callgraph edge for each existing
	// address-taken concrete type implementing I.
	iinfo := r.implementations(I)

	// Wait for initialization to complete
	<-iinfo.initDone

	iinfo.implementations.Iterate(func(C types.Type, v any) {
		r.addInvokeEdge(site, C)
	})
}

// ---------- main algorithm ----------

// visitFunc processes function f.
func (r *rta) visitFunc(f *ssa.Function) {
	var space [32]*ssa.Value // preallocate space for common case

	for _, b := range f.Blocks {
		for _, instr := range b.Instrs {
			rands := instr.Operands(space[:0])

			switch instr := instr.(type) {
			case ssa.CallInstruction:
				call := instr.Common()
				if call.IsInvoke() {
					r.visitInvoke(instr)
				} else if g := call.StaticCallee(); g != nil {
					r.addEdge(f, instr, g, false)
				} else if _, ok := call.Value.(*ssa.Builtin); !ok {
					r.visitDynCall(instr)
				}

				// Ignore the call-position operand when
				// looking for address-taken Functions.
				// Hack: assume this is rands[0].
				rands = rands[1:]

			case *ssa.MakeInterface:
				// Converting a value of type T to an
				// interface materializes its runtime
				// type, allowing any of its exported
				// methods to be called though reflection.
				r.addRuntimeType(instr.X.Type(), false)
			}

			// Process all address-taken functions.
			for _, op := range rands {
				if g, ok := (*op).(*ssa.Function); ok {
					r.visitAddrTakenFunc(g)
				}
			}
		}
	}
}

// RTA is an RTA implementation using a work-item based approach with deduplication.
type RTA struct{}

// New creates a new RTA instance.
func New() *RTA {
	return &RTA{}
}

// Analyze performs Rapid Type Analysis, starting at the specified root
// functions.  It returns nil if no roots were specified.
//
// The root functions must be one or more entrypoints (main and init
// functions) of a complete SSA program, with function bodies for all
// dependencies, constructed with the [ssa.InstantiateGenerics] mode
// flag.
//
// If buildCallGraph is true, Result.CallGraph will contain a call
// graph; otherwise, only the other fields (reachable functions) are
// populated.
func (a *RTA) Analyze(roots []*ssa.Function, buildCallGraph bool, numWorkers int) kumo.Result {
	if len(roots) == 0 {
		return nil
	}

	// Default to 1 worker
	if numWorkers <= 0 {
		numWorkers = 1
	}

	r := &rta{
		result:   &Result{},
		prog:     roots[0].Prog,
		work:     lockfree.NewMSQueue[workItem](),
		workDone: make(chan struct{}),
	}

	if buildCallGraph {
		r.result.callGraph = callgraph.New(roots[0])
	}

	// Grab ssa.Function for (*reflect.Value).Call,
	// if "reflect" is among the dependencies.
	if reflectPkg := r.prog.ImportedPackage("reflect"); reflectPkg != nil {
		reflectValue := reflectPkg.Members["Value"].(*ssa.Type)
		r.reflectValueCall = r.prog.LookupMethod(reflectValue.Object().Type(), reflectPkg.Pkg, "Call")
	}

	hasher := typeutil.MakeHasher()
	r.result.runtimeTypes.SetHasher(hasher)
	r.addrTakenFuncsBySig.SetHasher(hasher)
	r.dynCallSites.SetHasher(hasher)
	r.invokeSites.SetHasher(hasher)
	r.concreteTypes.SetHasher(hasher)
	r.interfaceTypes.SetHasher(hasher)

	// Start workers
	for i := range numWorkers {
		go func(i int) {
			defaultCount := 0
			for {
				select {
				case _, ok := <-r.workDone:
					if !ok {
						return
					}
				default:
					work, ok := r.work.Dequeue()
					if ok {
						work.doWork(r)
						r.workWg.Done()
						defaultCount = 0
					} else {
						// The worker goroutines use a select statement with a default case that
						// continuously calls Dequeue() when the queue is empty. This creates a tight
						// loop that consumes CPU unnecessarily when no work is available, as the
						// goroutine will repeatedly try to dequeue without any blocking or delay.
						//
						// To circumvent this, if we hit default repeatedly for 10 times, yield control
						// back to the runtime
						defaultCount++
						if defaultCount >= 10 {
							runtime.Gosched()
							defaultCount = 0
						}
					}
				}
			}
		}(i)
	}

	// Add the initial work
	for _, root := range roots {
		r.addReachable(root, false)
	}

	// Wait for RTA-related work to complete
	r.workWg.Wait()

	// De-duplicate edges, using the worker pool with batching
	r.processNodesInBatches(numWorkers)

	// Signal workers to shut down
	close(r.workDone)

	// Copy sync.Map reference to result - no conversion needed until Materialize()

	return r.result
}

// processNodesInBatches processes deduplication work items in batches to avoid
// doubling memory usage by limiting the number of work items queued at once.
// It uses a batch size of 2*numWorkers.
// A buffered channel acts as both a semaphore and a slice pool.
func (r *rta) processNodesInBatches(numWorkers int) {
	batchSize := numWorkers * 2

	// Create semaphore channel that holds slices for recycling
	semaphore := make(chan []*callgraph.Node, batchSize)

	// Pre-populate with empty slices
	for i := 0; i < batchSize; i++ {
		semaphore <- make([]*callgraph.Node, 0, batchSize)
	}

	// Get a slice from the semaphore (blocks if none available)
	batch := <-semaphore

	// Process nodes directly from the map without creating an intermediate slice
	for _, node := range r.result.callGraph.Nodes {
		batch = append(batch, node)

		// When batch is full, submit it and get a new one
		if len(batch) >= batchSize {
			r.addToWorklist(&deduplicateEdgesWorkItem{nodes: batch, semaphore: semaphore})

			// Get next slice from semaphore (blocks if all are in use)
			batch = <-semaphore
		}
	}

	// Submit remaining nodes if any
	if len(batch) > 0 {
		r.addToWorklist(&deduplicateEdgesWorkItem{nodes: batch, semaphore: semaphore})
	} else {
		// Return unused slice to semaphore
		semaphore <- batch
	}

	// Wait for all batches to complete by draining the semaphore
	for i := 0; i < batchSize; i++ {
		<-semaphore
	}
}

// interfaces() and implementations()
//
// In both of these methods, if we are encoutering the concrete type/interface
// for the first time, we broadcast the existence of a
// concreteTypeInfo/interfaceTypeInfo (via a LoadOrStore) and only then
// populate `implements`/`implementations` fields (via Range). This order
// ensures that no work item gets dropped; either a consumer of `interfaces()`
// will end up adding it, or a consumer of `implementations()` will add it.

// interfaces(C) returns all currently known interfaces implemented by C.
func (r *rta) interfaces(C types.Type) *concreteTypeInfo {
	switch C.(type) {
	case *types.Tuple,
		*types.Array,
		*types.Slice,
		*types.Chan,
		*types.Signature,
		*types.Map:
		cinfo := &concreteTypeInfo{
			C:        C,
			initDone: make(chan struct{}),
		}
		close(cinfo.initDone)
		return cinfo
	}

	// Create an info for C the first time we see it.
	var cinfo *concreteTypeInfo
	if v := r.concreteTypes.At(C); v != nil {
		cinfo = v.(*concreteTypeInfo)
	} else {
		mset := r.prog.MethodSets.MethodSet(C)
		cinfo = &concreteTypeInfo{
			C:        C,
			mset:     mset,
			fprint:   fingerprint(mset),
			initDone: make(chan struct{}),
		}
		if oldcinfo, loaded := r.concreteTypes.LoadOrStore(C, cinfo); !loaded {
			// Ascertain set of interfaces C implements
			// and update the 'implements' relation.
			r.interfaceTypes.Iterate(func(I types.Type, v any) {
				iinfo := v.(*interfaceTypeInfo)
				if I := types.Unalias(I).(*types.Interface); implements(cinfo, iinfo) {
					iinfo.implementations.Set(C, struct{}{})
					// This is okay because consumers are expected to not use cinfo.implements until cinfo.initDone is closed
					cinfo.implements.SetNoLock(I, struct{}{})
				}
			})

			close(cinfo.initDone)
		} else {
			return oldcinfo.(*concreteTypeInfo)
		}
	}
	return cinfo
}

// implementations(I) returns all currently known concrete types that implement I.
func (r *rta) implementations(I *types.Interface) *interfaceTypeInfo {
	// Create an info for I the first time we see it.
	var iinfo *interfaceTypeInfo
	if v := r.interfaceTypes.At(I); v != nil {
		iinfo = v.(*interfaceTypeInfo)
	} else {
		mset := r.prog.MethodSets.MethodSet(I)
		iinfo = &interfaceTypeInfo{
			I:        I,
			mset:     mset,
			fprint:   fingerprint(mset),
			initDone: make(chan struct{}),
		}
		if oldiinfo, loaded := r.interfaceTypes.LoadOrStore(I, iinfo); !loaded {
			// Ascertain set of concrete types that implement I
			// and update the 'implements' relation.

			r.concreteTypes.Iterate(func(C types.Type, v any) {
				cinfo := v.(*concreteTypeInfo)
				if implements(cinfo, iinfo) {
					cinfo.implements.Set(I, struct{}{})
					// This is okay because consumers are expected to not use iinfo.implementations until iinfo.initDone is closed
					iinfo.implementations.SetNoLock(C, struct{}{})
				}
			})

			close(iinfo.initDone)
		} else {
			return oldiinfo.(*interfaceTypeInfo)
		}
	}
	return iinfo
}

// addRuntimeType is called for each concrete type that can be the
// dynamic type of some interface or reflect.Value.
// Adapted from needMethods in go/ssa/builder.go
func (r *rta) addRuntimeType(T types.Type, skip bool) {
	// Never record aliases.

	T = types.Unalias(T)

	if prev, loaded := r.result.runtimeTypes.LoadOrStore(T, skip); loaded {
		// Type already exists, update if we need to change skip from true to false
		// `skip` only moves from true to false, not the other way around.
		if !skip && prev.(bool) {
			r.result.runtimeTypes.Set(T, skip)
		}
		return
	}
	// Type was newly stored, continue with processing

	mset := r.prog.MethodSets.MethodSet(T)

	if _, ok := T.Underlying().(*types.Interface); !ok {
		// T is a new concrete type.
		for i, n := 0, mset.Len(); i < n; i++ {
			sel := mset.At(i)
			m := sel.Obj()

			if m.Exported() {
				// Exported methods are always potentially callable via reflection.
				r.addReachable(r.prog.MethodValue(sel), true)
			}
		}

		// Add callgraph edge for each existing dynamic
		// "invoke"-mode call via that interface.
		cinfo := r.interfaces(T)

		// Wait for initialization to complete
		<-cinfo.initDone

		cinfo.implements.Iterate(func(k types.Type, v any) {
			I := k.(*types.Interface)
			val, _ := r.invokeSites.LoadOrStore(I, &callSites{})
			s := val.(*callSites)
			s.sites.Range(func(key any, value any) bool {
				site := key.(ssa.CallInstruction)
				r.addInvokeEdge(site, T)
				return true
			})
		})
	}

	// Precondition: T is not a method signature (*Signature with Recv()!=nil).
	// Recursive case: skip => don't call makeMethods(T).
	// Each package maintains its own set of types it has visited.

	var n *types.Named
	switch T := types.Unalias(T).(type) {
	case *types.Named:
		n = T
	case *types.Pointer:
		n, _ = types.Unalias(T.Elem()).(*types.Named)
	}
	if n != nil {
		owner := n.Obj().Pkg()
		if owner == nil {
			return // built-in error type
		}
	}

	// Recursion over signatures of each exported method.
	for i := 0; i < mset.Len(); i++ {
		if mset.At(i).Obj().Exported() {
			sig := mset.At(i).Type().(*types.Signature)
			r.addRuntimeType(sig.Params(), true)  // skip the Tuple itself
			r.addRuntimeType(sig.Results(), true) // skip the Tuple itself
		}
	}

	switch t := T.(type) {
	case *types.Alias:
		panic("unreachable")

	case *types.Basic:
		// nop

	case *types.Interface:
		// nop---handled by recursion over method set.

	case *types.Pointer:
		r.addRuntimeType(t.Elem(), false)

	case *types.Slice:
		r.addRuntimeType(t.Elem(), false)

	case *types.Chan:
		r.addRuntimeType(t.Elem(), false)

	case *types.Map:
		r.addRuntimeType(t.Key(), false)
		r.addRuntimeType(t.Elem(), false)

	case *types.Signature:
		if t.Recv() != nil {
			panic(fmt.Sprintf("Signature %s has Recv %s", t, t.Recv()))
		}
		r.addRuntimeType(t.Params(), true)  // skip the Tuple itself
		r.addRuntimeType(t.Results(), true) // skip the Tuple itself

	case *types.Named:
		// A pointer-to-named type can be derived from a named
		// type via reflection.  It may have methods too.
		r.addRuntimeType(types.NewPointer(T), false)

		// Consider 'type T struct{S}' where S has methods.
		// Reflection provides no way to get from T to struct{S},
		// only to S, so the method set of struct{S} is unwanted,
		// so set 'skip' flag during recursion.
		r.addRuntimeType(t.Underlying(), true)

	case *types.Array:
		r.addRuntimeType(t.Elem(), false)

	case *types.Struct:
		for i, n := 0, t.NumFields(); i < n; i++ {
			r.addRuntimeType(t.Field(i).Type(), false)
		}

	case *types.Tuple:
		for i, n := 0, t.Len(); i < n; i++ {
			r.addRuntimeType(t.At(i).Type(), false)
		}

	default:
		panic(T)
	}
}

// fingerprint returns a bitmask with one bit set per method id,
// enabling 'implements' to quickly reject most candidates.
func fingerprint(mset *types.MethodSet) uint64 {
	var space [64]byte
	var mask uint64
	for i := 0; i < mset.Len(); i++ {
		method := mset.At(i).Obj()
		sig := method.Type().(*types.Signature)
		sum := crc32.ChecksumIEEE(fmt.Appendf(space[:], "%s/%d/%d",
			method.Id(),
			sig.Params().Len(),
			sig.Results().Len()))
		mask |= 1 << (sum % 64)
	}
	return mask
}

// implements reports whether types.Implements(cinfo.C, iinfo.I),
// but more efficiently.
func implements(cinfo *concreteTypeInfo, iinfo *interfaceTypeInfo) (got bool) {
	// The concrete type must have at least the methods
	// (bits) of the interface type. Use a bitwise subset
	// test to reject most candidates quickly.
	return iinfo.fprint & ^cinfo.fprint == 0 && types.Implements(cinfo.C, iinfo.I)
}

// dedupEdges is a helper function to de-duplicate the edges of a node in-place
func dedupEdges(edges []*callgraph.Edge) []*callgraph.Edge {
	// For small edge counts, use O(N^2) algorithm to avoid map allocation
	if len(edges) < _dedupEdgesSmallThreshold {
		return dedupEdgesSmall(edges)
	}
	return dedupEdgesLarge(edges)
}

// dedupEdgesLarge deduplicates edges in-place using a map for larger edge counts
func dedupEdgesLarge(edges []*callgraph.Edge) []*callgraph.Edge {
	seen := make(map[callgraph.Edge]int)
	writeIdx := 0

	for _, edge := range edges {
		if edge == nil {
			continue
		}

		if idx, ok := seen[*edge]; !ok {
			// First time seeing this edge, add it
			seen[*edge] = writeIdx
			edges[writeIdx] = edge
			writeIdx++
		} else {
			// Already seen, keep the one with smaller pointer value
			existing := edges[idx]
			if uintptr(unsafe.Pointer(edge)) < uintptr(unsafe.Pointer(existing)) {
				edges[idx] = edge
			}
		}
	}

	return edges[:writeIdx]

}

// dedupEdgesSmall deduplicates edges in-place using O(N^2) algorithm for small edge counts
// to avoid map allocation overhead
func dedupEdgesSmall(edges []*callgraph.Edge) []*callgraph.Edge {
	writeIdx := 0

	for _, edge := range edges {
		if edge == nil {
			continue
		}

		// Check if this edge is already in the deduplicated portion
		found := false
		for i := 0; i < writeIdx; i++ {
			if *edge == *edges[i] {
				found = true
				// Keep the edge with smaller pointer value
				if uintptr(unsafe.Pointer(edge)) < uintptr(unsafe.Pointer(edges[i])) {
					edges[i] = edge
				}
				break
			}
		}

		if !found {
			edges[writeIdx] = edge
			writeIdx++
		}
	}

	return edges[:writeIdx]
}

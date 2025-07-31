// Parallel RTA
//
// Code is heavily borrowed from Go's RTA package. We make some changes to make
// it parallel.

package prta

import (
	"fmt"
	// "strings"
	"go/types"
	"hash/crc32"
	"hash/fnv"
	"sort"
	"sync"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/types/typeutil"

	"github.com/1ntEgr8/prta/lockfree"
)

// A Result holds the results of Rapid Type Analysis, which includes the
// set of reachable functions/methods, runtime types, and the call graph.
type Result struct {
	// CallGraph is the discovered callgraph.
	// It does not include edges for calls made via reflection.
	CallGraph *callgraph.Graph

	// Reachable contains the set of reachable functions and methods.
	// This includes exported methods of runtime types, since
	// they may be accessed via reflection.
	// The value indicates whether the function is address-taken.
	//
	// (We wrap the bool in a struct to avoid inadvertent use of
	// "if Reachable[f] {" to test for set membership.)
	Reachable sync.Map // map[*ssa.Function]struct{ AddrTaken bool }

	// RuntimeTypes contains the set of types that are needed at
	// runtime, for interfaces or reflection.
	//
	// The value indicates whether the type is inaccessible to reflection.
	// Consider:
	// 	type A struct{B}
	// 	fmt.Println(new(A))
	// Types *A, A and B are accessible to reflection, but the unnamed
	// type struct{B} is not.
	RuntimeTypes TypeMap
}

// Working state of the RTA algorithm.
type rta struct {
	result *Result

	prog *ssa.Program

	reflectValueCall *ssa.Function // (*reflect.Value).Call, iff part of prog

	work     *lockfree.MSQueue[*ssa.Function]
	workDone chan struct{}
	workWg   sync.WaitGroup

	// addrTakenFuncsBySig contains all address-taken *Functions, grouped by signature.
	// Keys are *types.Signature, values are map[*ssa.Function]bool sets.
	addrTakenFuncsBySig TypeMap

	// dynCallSites contains all dynamic "call"-mode call sites, grouped by signature.
	// Keys are *types.Signature, values are unordered []ssa.CallInstruction.
	dynCallSites TypeMap

	// invokeSites contains all "invoke"-mode call sites, grouped by interface.
	// Keys are *types.Interface (never *types.Named),
	// Values are unordered []ssa.CallInstruction sets.
	invokeSites TypeMap

	// The following two maps together define the subset of the
	// m:n "implements" relation needed by the algorithm.

	// concreteTypes maps each concrete type to information about it.
	// Keys are types.Type, values are *concreteTypeInfo.
	// Only concrete types used as MakeInterface operands are included.
	concreteTypes TypeMap

	// interfaceTypes maps each interface type to information about it.
	// Keys are *types.Interface, values are *interfaceTypeInfo.
	// Only interfaces used in "invoke"-mode CallInstructions are included.
	interfaceTypes TypeMap

	callgraphLk sync.Mutex

	aliasLk sync.Mutex

	// Current step count for debugging
	currentStep int

	concreteTypeInfoTbl  map[types.Type]*concreteTypeInfo
	interfaceTypeInfoTbl map[*types.Interface]*interfaceTypeInfo
}

type concreteTypeInfo struct {
	C          types.Type
	mset       *types.MethodSet
	fprint     uint64             // fingerprint of method set
	implements []*types.Interface // unordered set of implemented interfaces

	initialized bool
	mu          sync.Mutex // protects implements slice
}

type interfaceTypeInfo struct {
	I               *types.Interface
	mset            *types.MethodSet
	fprint          uint64
	implementations []types.Type // unordered set of concrete implementations

	initialized bool
	mu          sync.Mutex // protects implementations slice
}

// TODO (could be replaced with a sync.Map)
type callSites struct {
	sites []ssa.CallInstruction
	lk    sync.Mutex
}

// addReachable marks a function as potentially callable at run-time,
// and ensures that it gets processed.
func (r *rta) addReachable(f *ssa.Function, addrTaken bool) {
	existing, loaded := r.result.Reachable.LoadOrStore(f, struct{ AddrTaken bool }{addrTaken})
	if loaded && addrTaken && !existing.(struct{ AddrTaken bool }).AddrTaken {
		// Need to update existing entry to set AddrTaken=true
		r.result.Reachable.Store(f, struct{ AddrTaken bool }{true})
	}
	if !loaded {
		// First time seeing f.  Add it to the worklist.
		r.addToWorklist(f)
	}
}

func (r *rta) addToWorklist(f *ssa.Function) {
	r.workWg.Add(1)
	r.work.Enqueue(f)
}

// addEdge adds the specified call graph edge, and marks it reachable.
// addrTaken indicates whether to mark the callee as "address-taken".
// site is nil for calls made via reflection.
func (r *rta) addEdge(caller *ssa.Function, site ssa.CallInstruction, callee *ssa.Function, addrTaken bool) {
	r.addReachable(callee, addrTaken)

	if g := r.result.CallGraph; g != nil {
		if caller == nil {
			panic(site)
		}

		r.callgraphLk.Lock()

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

	// if funcs == nil {
	// 	funcs = make(map[*ssa.Function]bool)
	// 	r.addrTakenFuncsBySig.Set(S, funcs)
	// }

	if _, loaded := funcs.LoadOrStore(f, true); !loaded {
		// First time seeing f.

		// If we've seen any dyncalls of this type, mark it reachable,
		// and add call graph edges.
		val, _ := r.dynCallSites.LoadOrStore(S, &callSites{})
		s := val.(*callSites)
		s.lk.Lock()
		for _, site := range s.sites {
			r.addEdge(site.Parent(), site, f, true)
		}
		s.lk.Unlock()

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
	val, _ := r.dynCallSites.LoadOrStore(S, &callSites{})
	s := val.(*callSites)
	s.lk.Lock()
	s.sites = append(s.sites, site)
	s.lk.Unlock()

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

	s.lk.Lock()
	s.sites = append(s.sites, site)
	s.lk.Unlock()

	// r.invokeSites.Set(I, append(sites, site))

	// Add callgraph edge for each existing
	// address-taken concrete type implementing I.
	iinfo := r.implementations(I)
	for _, C := range iinfo.implementations {
		r.addInvokeEdge(site, C)
	}
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

func (r *rta) populateTable(pkgs []*packages.Package) {
	concreteTypeInfoTbl := make(map[types.Type]*concreteTypeInfo)
	interfaceTypeInfoTbl := make(map[*types.Interface]*interfaceTypeInfo)

	fmt.Println("Populating table...")

	for _, pkg := range pkgs {
		fmt.Printf("Package %v\n", pkg)
		for _, obj := range pkg.TypesInfo.Defs {
			if obj == nil {
				continue
			}
			ty := types.Unalias(obj.Type())
			if types.IsInterface(ty) {
				I := ty.Underlying().(*types.Interface)
				mset := r.prog.MethodSets.MethodSet(I)
				interfaceTypeInfoTbl[I] = &interfaceTypeInfo{
					I:      I,
					mset:   mset,
					fprint: fingerprint(mset),
				}
			} else {
				C := ty
				mset := r.prog.MethodSets.MethodSet(C)
				concreteTypeInfoTbl[C] = &concreteTypeInfo{
					C:      C,
					mset:   mset,
					fprint: fingerprint(mset),
				}
			}
		}
	}

	r.concreteTypeInfoTbl = concreteTypeInfoTbl
	r.interfaceTypeInfoTbl = interfaceTypeInfoTbl

	fmt.Println("DONE")
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
func Analyze(roots []*ssa.Function, pkgs []*packages.Package, buildCallGraph bool, numWorkers int) *Result {
	if len(roots) == 0 {
		return nil
	}

	r := &rta{
		result:   &Result{Reachable: sync.Map{}},
		prog:     roots[0].Prog,
		work:     lockfree.NewMSQueue[*ssa.Function](),
		workDone: make(chan struct{}),
	}

	if buildCallGraph {
		// TODO(adonovan): change callgraph API to eliminate the
		// notion of a distinguished root node.  Some callgraphs
		// have many roots, or none.
		r.result.CallGraph = callgraph.New(roots[0])
	}

	// Grab ssa.Function for (*reflect.Value).Call,
	// if "reflect" is among the dependencies.
	if reflectPkg := r.prog.ImportedPackage("reflect"); reflectPkg != nil {
		reflectValue := reflectPkg.Members["Value"].(*ssa.Type)
		r.reflectValueCall = r.prog.LookupMethod(reflectValue.Object().Type(), reflectPkg.Pkg, "Call")
	}

	hasher := typeutil.MakeHasher()
	r.result.RuntimeTypes.SetHasher(hasher)
	r.addrTakenFuncsBySig.SetHasher(hasher)
	r.dynCallSites.SetHasher(hasher)
	r.invokeSites.SetHasher(hasher)
	r.concreteTypes.SetHasher(hasher)
	r.interfaceTypes.SetHasher(hasher)

	// Pre-populate table
	r.populateTable(pkgs)

	// Start workers
	for i := range numWorkers {
		go func(i int) {
			for {
				select {
				case _, ok := <-r.workDone:
					if !ok {
						return
					}
				default:
					work, ok := r.work.Dequeue()
					if ok {
						r.visitFunc(work)
						r.workWg.Done()
					}
				}
			}
		}(i)
	}

	// Add the initial work
	for _, root := range roots {
		r.addReachable(root, false)
	}

	// Wait for all work to complete
	r.workWg.Wait()

	// Signal workers to shut down
	close(r.workDone)

	return r.result
}

// interfaces(C) returns all currently known interfaces implemented by C.
func (r *rta) interfaces(C types.Type) *concreteTypeInfo {
	cinfo, ok := r.concreteTypeInfoTbl[C]
	if !ok {
		panic(fmt.Sprintf("Concrete type not found in table %v", C))
	}
	for !cinfo.initialized {
		cinfo.mu.Lock()
		for I, iinfo := range r.interfaceTypeInfoTbl {
			if implements(cinfo, iinfo) {
				cinfo.implements = append(cinfo.implements, I)
			}
		}
		cinfo.initialized = true
		cinfo.mu.Unlock()
	}
	return cinfo
}

// implementations(I) returns all currently known concrete types that implement I.
func (r *rta) implementations(I *types.Interface) *interfaceTypeInfo {
	iinfo, ok := r.interfaceTypeInfoTbl[I]
	if !ok {
		panic(fmt.Sprintf("Interface not found in table! %v", I))
	}
	for !iinfo.initialized {
		iinfo.mu.Lock()
		for C, cinfo := range r.concreteTypeInfoTbl {
			if implements(cinfo, iinfo) {
				iinfo.implementations = append(iinfo.implementations, C)
			}
		}
		iinfo.initialized = true
		iinfo.mu.Unlock()
	}
	return iinfo
}

func (r *rta) lockedUnalias(T types.Type) types.Type {
	// r.aliasLk.Lock()
	OT := types.Unalias(T)
	// r.aliasLk.Unlock()
	return OT
}

// addRuntimeType is called for each concrete type that can be the
// dynamic type of some interface or reflect.Value.
// Adapted from needMethods in go/ssa/builder.go
func (r *rta) addRuntimeType(T types.Type, skip bool) {
	// Never record aliases.

	T = r.lockedUnalias(T)

	if prev, loaded := r.result.RuntimeTypes.LoadOrStore(T, skip); loaded {
		// Type already exists, update if we need to change skip from true to false
		if !skip && prev.(bool) {
			r.result.RuntimeTypes.Set(T, skip)
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
		for _, I := range cinfo.implements {
			val, _ := r.invokeSites.LoadOrStore(I, &callSites{})
			s := val.(*callSites)
			s.lk.Lock()
			for _, site := range s.sites {
				r.addInvokeEdge(site, T)
			}
			s.lk.Unlock()
		}
	}

	// Precondition: T is not a method signature (*Signature with Recv()!=nil).
	// Recursive case: skip => don't call makeMethods(T).
	// Each package maintains its own set of types it has visited.

	var n *types.Named
	switch T := r.lockedUnalias(T).(type) {
	case *types.Named:
		n = T
	case *types.Pointer:
		n, _ = r.lockedUnalias(T.Elem()).(*types.Named)
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

// NodeHash computes a hash for a callgraph.Node incorporating its function and all edges
func NodeHash(node *callgraph.Node) uint64 {
	if node == nil || node.Func == nil {
		return 0
	}

	h := fnv.New64a()

	// Hash the function name
	h.Write([]byte(node.Func.String()))

	// Hash incoming edges (deduplicated)
	inEdgeSet := make(map[string]bool)
	for _, edge := range node.In {
		edgeStr := edgeHash(edge)
		inEdgeSet[edgeStr] = true
	}
	inEdges := make([]string, 0, len(inEdgeSet))
	for edgeStr := range inEdgeSet {
		inEdges = append(inEdges, edgeStr)
	}
	sort.Strings(inEdges) // Ensure deterministic order
	for _, edgeStr := range inEdges {
		h.Write([]byte("IN:"))
		h.Write([]byte(edgeStr))
	}

	// Hash outgoing edges (deduplicated)
	outEdgeSet := make(map[string]bool)
	for _, edge := range node.Out {
		edgeStr := edgeHash(edge)
		outEdgeSet[edgeStr] = true
	}
	outEdges := make([]string, 0, len(outEdgeSet))
	for edgeStr := range outEdgeSet {
		outEdges = append(outEdges, edgeStr)
	}
	sort.Strings(outEdges) // Ensure deterministic order
	for _, edgeStr := range outEdges {
		h.Write([]byte("OUT:"))
		h.Write([]byte(edgeStr))
	}

	return h.Sum64()
}

// edgeHash computes a hash string for a callgraph.Edge
func edgeHash(edge *callgraph.Edge) string {
	if edge == nil {
		return ""
	}

	callerStr := ""
	if edge.Caller != nil && edge.Caller.Func != nil {
		callerStr = edge.Caller.Func.String()
	}

	calleeStr := ""
	if edge.Callee != nil && edge.Callee.Func != nil {
		calleeStr = edge.Callee.Func.String()
	}

	siteStr := ""
	if edge.Site != nil {
		siteStr = edge.Site.String()
	}

	return fmt.Sprintf("%s->%s@%s", callerStr, calleeStr, siteStr)
}

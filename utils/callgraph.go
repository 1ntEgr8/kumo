package utils

import (
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/ssa"
)

type nodeEntry struct {
	node *callgraph.Node
	lock *sync.Mutex
}

// A CallGraph represents a call graph.
//
// A graph may contain nodes that are not reachable from the root.
// If the call graph is sound, such nodes indicate unreachable
// functions.
type ConcurrentCallGraph struct {
	root *ssa.Function

	nodes  sync.Map // map[*ssa.Function]*nodeEntry
	nextID int32    // atomic counter for node IDs
}

// New returns a new CallGraph with the specified (optional) root node.
func NewConcurrentCallGraph(root *ssa.Function) *ConcurrentCallGraph {
	g := &ConcurrentCallGraph{
		root: root,
	}
	// Create the root node
	rootNode := &callgraph.Node{Func: root, ID: 0}
	entry := &nodeEntry{
		node: rootNode,
		lock: &sync.Mutex{},
	}
	g.nodes.Store(root, entry)
	g.nextID = 1
	return g
}

// CreateNode returns the Node for fn, creating it if not present.
// The root node may have fn=nil.
func (g *ConcurrentCallGraph) CreateNode(fn *ssa.Function) *callgraph.Node {
	// Try to load existing node
	if val, ok := g.nodes.Load(fn); ok {
		return val.(*nodeEntry).node
	}

	// Generate new ID atomically
	id := int(atomic.AddInt32(&g.nextID, 1) - 1)

	// Create new node entry with both node and lock
	newNode := &callgraph.Node{Func: fn, ID: id}
	entry := &nodeEntry{
		node: newNode,
		lock: &sync.Mutex{},
	}

	actual, _ := g.nodes.LoadOrStore(fn, entry)
	return actual.(*nodeEntry).node
}

// AddEdge adds the edge (caller, site, callee) to the call graph.
// Elimination of duplicate edges is the caller's responsibility.
func (g *ConcurrentCallGraph) AddCallGraphEdge(caller *callgraph.Node, site ssa.CallInstruction, callee *callgraph.Node) {
	e := &callgraph.Edge{
		Caller: caller,
		Site:   site,
		Callee: callee,
	}

	// Look up callee's entry by its function
	val, ok := g.nodes.Load(callee.Func)
	if !ok {
		panic(fmt.Sprintf("Could not find callee: %v\n", callee))
	}
	calleeEntry := val.(*nodeEntry)
	calleeEntry.lock.Lock()
	callee.In = append(callee.In, e)
	calleeEntry.lock.Unlock()

	// Look up caller's entry by its function
	val, ok = g.nodes.Load(caller.Func)
	if !ok {
		panic(fmt.Sprintf("Could not find caller: %v\n", caller))
	}
	callerEntry := val.(*nodeEntry)
	callerEntry.lock.Lock()
	caller.Out = append(caller.Out, e)
	callerEntry.lock.Unlock()
}

// GetGraph materializes and returns a callgraph.Graph by iterating over
// the sync.Map and reconstructing the graph.
func (g *ConcurrentCallGraph) GetGraph() *callgraph.Graph {
	// Create the graph with the root
	var rootNode *callgraph.Node
	if val, ok := g.nodes.Load(g.root); ok {
		rootNode = val.(*nodeEntry).node
	}

	graph := &callgraph.Graph{
		Root:  rootNode,
		Nodes: make(map[*ssa.Function]*callgraph.Node),
	}

	// Iterate over all nodes in the sync.Map and add them to the graph
	g.nodes.Range(func(key, value any) bool {
		fn := key.(*ssa.Function)
		entry := value.(*nodeEntry)
		graph.Nodes[fn] = entry.node
		return true
	})

	return graph
}

package lockfree

import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

// Taken from https://www.cs.rochester.edu/~scott/papers/1996_PODC_queues.pdf
// structure pointer_t { ptr: pointer to node_t, count: unsigned integer }
// # A pointer structure that includes a reference to a node and a counter to prevent the ABA problem.

// structure node_t { value: data type, next: pointer_t }
// # A node contains a value and a next pointer, which links it to the next node in the queue.

// structure queue_t { Head: pointer_t, Tail: pointer_t }
// # The queue maintains pointers to both the head and tail.

// initialize(Q: pointer to queue_t)
//     node = new node()             # Allocate a free node
//     node->next.ptr = NULL         # Make it the only node in the linked list
//     Q->Head = Q->Tail = node      # Both Head and Tail point to the dummy node

// enqueue(Q: pointer to queue_t, value: data type)
// E1: node = new node()             # Allocate a new node from the free list
// E2: node->value = value           # Copy enqueued value into node
// E3: node->next.ptr = NULL         # Set next pointer of the new node to NULL
// E4: loop                          # Keep trying until enqueue is done
// E5:   tail = Q->Tail              # Read Tail pointer and count together
// E6:   next = tail.ptr->next       # Read next pointer and count fields together
// E7:   if tail == Q->Tail          # Are tail and next consistent?
// E8:      if next.ptr == NULL      # Was Tail pointing to the last node?
// E9:         if CAS(&tail.ptr->next, next, <node, next.count+1>)
//                                    # Try to link new node at the end of the linked list
// E10:           break              # Enqueue is done, exit loop
// E11:        endif
// E12:     else                     # Tail was not pointing to the last node
// E13:        CAS(&Q->Tail, tail, <next.ptr, tail.count+1>)
//                                    # Try to swing Tail to the next node
// E14:     endif
// E15:  endif
// E16: endloop
// E17: CAS(&Q->Tail, tail, <node, tail.count+1>)
//                                    # Enqueue is done. Try to swing Tail to the inserted node
// dequeue(Q: pointer to queue_t, pvalue: pointer to data type): boolean
// D1: loop                          # Keep trying until dequeue is done
// D2:   head = Q->Head              # Read Head pointer and count together
// D3:   tail = Q->Tail              # Read Tail pointer
// D4:   next = head->next           # Read Head's next pointer
// D5:   if head == Q->Head          # Are head, tail, and next consistent?
// D6:      if head.ptr == tail.ptr  # Is queue empty or Tail falling behind?
// D7:         if next.ptr == NULL   # Is queue empty?
// D8:            return FALSE       # Queue is empty, couldn't dequeue
// D9:         endif
// D10:        CAS(&Q->Tail, tail, <next.ptr, tail.count+1>)
//                                    # Tail is falling behind, try to advance it
// D11:        # No need to deal with Tail
// D12:     else
// D13:        *pvalue = next.ptr->value
//                                    # Read value before CAS, otherwise another dequeue might free the next node
// D14:        if CAS(&Q->Head, head, <next.ptr, head.count+1>)
//                                    # Try to swing Head to the next node
// D15:           break               # Dequeue is done, exit loop
// D16:        endif
// D17:     endif
// D18:  endif
// D19: endloop
// D20: free(head.ptr)               # It is safe now to free the old dummy node
// D21: return TRUE                  # Queue was not empty, dequeue succeeded

// dequeue(Q: pointer to queue_t, pvalue: pointer to data type): boolean
// D1: loop                          # Keep trying until dequeue is done
// D2:   head = Q->Head              # Read Head pointer and count together
// D3:   tail = Q->Tail              # Read Tail pointer
// D4:   next = head->next           # Read Head's next pointer
// D5:   if head == Q->Head          # Are head, tail, and next consistent?
// D6:      if head.ptr == tail.ptr  # Is queue empty or Tail falling behind?
// D7:         if next.ptr == NULL   # Is queue empty?
// D8:            return FALSE       # Queue is empty, couldn't dequeue
// D9:         endif
// D10:        CAS(&Q->Tail, tail, <next.ptr, tail.count+1>)
//                                    # Tail is falling behind, try to advance it
// D11:        # No need to deal with Tail
// D12:     else
// D13:        *pvalue = next.ptr->value
//                                    # Read value before CAS, otherwise another dequeue might free the next node
// D14:        if CAS(&Q->Head, head, <next.ptr, head.count+1>)
//                                    # Try to swing Head to the next node
// D15:           break               # Dequeue is done, exit loop
// D16:        endif
// D17:     endif
// D18:  endif
// D19: endloop
// D20: free(head.ptr)               # It is safe now to free the old dummy node
// D21: return TRUE                  # Queue was not empty, dequeue succeeded

// node represents a single element in the queue.
type node[T any] struct {
	value T              // The actual value stored in the queue node
	next  unsafe.Pointer // Pointer to the next node in the queue
}

// MSQueue is a lock-free Michael-Scott queue.
type MSQueue[T any] struct {
	head unsafe.Pointer // Pointer to the head of the queue
	tail unsafe.Pointer // Pointer to the tail of the queue
}

const maxLoopTrips = 0xf

// NewMSQueue initializes a new lock-free queue.
func NewMSQueue[T any]() *MSQueue[T] {
	dummy := unsafe.Pointer(&node[T]{})          // Allocate a dummy node (ensures queue is never completely empty)
	return &MSQueue[T]{head: dummy, tail: dummy} // Head and Tail both start pointing to the dummy node
}

func yieldOnContention(trips int) {
	if trips&maxLoopTrips == maxLoopTrips {
		runtime.Gosched() // Yield the processor to allow other goroutines to proceed
	}
}

// Enqueue inserts an element into the queue.
func (q *MSQueue[T]) Enqueue(value T) {
	newNode := &node[T]{value: value} // Step E1: Allocate a new node from the free list
	looptrips := 0
	for {
		tail := (*node[T])(atomic.LoadPointer(&q.tail))      // Step E5: Read the tail pointer (atomic load)
		next := atomic.LoadPointer(&tail.next)               // Step E6: Read tail.next pointer
		if tail == (*node[T])(atomic.LoadPointer(&q.tail)) { // Step E7: Are tail and next consistent?
			if next == nil { // Step E8: Was Tail pointing to the last node?
				if atomic.CompareAndSwapPointer(&tail.next, nil, unsafe.Pointer(newNode)) { // Step E9: Try to link node at the end of the linked list
					atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(tail), unsafe.Pointer(newNode)) // Step E17: Swing Tail to the inserted node
					return                                                                               // Enqueue operation is complete
				}
			} else {
				// Step E12: Tail was not pointing to the last node, so help advance the tail
				atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(tail), next)
			}
		}
		looptrips++
		yieldOnContention(looptrips)
	}
}

// Dequeue removes and returns an element from the queue.
func (q *MSQueue[T]) Dequeue() (T, bool) {
	var zero T // Default zero value of type T (used when queue is empty)
	looptrips := 0
	for {
		head := (*node[T])(atomic.LoadPointer(&q.head))    // Step D2: Read Head pointer
		tail := (*node[T])(atomic.LoadPointer(&q.tail))    // Step D3: Read Tail pointer
		next := (*node[T])(atomic.LoadPointer(&head.next)) // Step D4: Read Head's next pointer

		if head == (*node[T])(atomic.LoadPointer(&q.head)) { // Step D5: Are head, tail, and next consistent?
			if head == tail { // Step D6: Is queue empty or is Tail falling behind?
				if next == nil { // Step D7: Is queue empty?
					return zero, false // Step D8: Queue is empty, couldn't dequeue
				}
				// Step D10: Tail is falling behind, try to advance it
				atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(tail), unsafe.Pointer(next))
			} else {
				// Step D13: Read value before CAS to avoid freeing memory prematurely
				value := next.value
				if atomic.CompareAndSwapPointer(&q.head, unsafe.Pointer(head), unsafe.Pointer(next)) { // Step D14: Try to swing Head to the next node
					return value, true // Step D15: Dequeue operation is complete
				}
			}
		}
		looptrips++
		yieldOnContention(looptrips)
	}
}

// IsEmpty checks if the queue is empty in a lock-free manner.
// Original paper did not have this.
func (q *MSQueue[T]) IsEmpty() bool {
	head := (*node[T])(atomic.LoadPointer(&q.head)) // Read head pointer atomically
	next := atomic.LoadPointer(&head.next)          // Read head.next pointer

	return next == nil // If next is nil, the queue is empty
}

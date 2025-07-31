package lockfree

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	numEnqueuers = 4
	numDequeuers = 4
	numItems     = 500000 // Number of items each enqueuer pushes
	debug        = false
)

// QueueItem represents the struct stored in MSQueue.
type QueueItem struct {
	EnqueuerID int // Unique ID for each enqueuer
	Value      int // Unique value, sorted within each enqueuer
}

// TestMSQueueConcurrentWithStruct tests MSQueue with multiple enqueuers and dequeuers using struct type.
func TestMSQueueConcurrentWithStruct(t *testing.T) {
	queue := NewMSQueue[QueueItem]() // Create an MSQueue of struct type

	var wgEnqueue sync.WaitGroup
	var wgDequeue sync.WaitGroup

	var enqueuersReady int32 = 0                   // Atomic counter to track enqueuer readiness
	var dequeuersReady int32 = 0                   // Atomic counter to track dequeuer readiness
	enqueuedCount := int32(0)                      // Track the number of elements enqueued
	dequeuedCount := int32(0)                      // Track the number of elements dequeued
	globalDequeuedData := map[QueueItem]struct{}{} // Map to store all dequeued elements safely
	mtx := sync.Mutex{}                            // Mutex to protect globalDequeuedData

	allEnqFinished := int32(0) // Atomic flag to indicate all enqueuers have finished

	// Step 1: Start dequeuers first
	for i := 0; i < numDequeuers; i++ {
		wgDequeue.Add(1)
		go func(id int) {
			atomic.AddInt32(&dequeuersReady, 1) // Mark this dequeuer as ready
			defer wgDequeue.Done()

			localDequeued := make(map[int][]QueueItem) // Track dequeued values per enqueuer ID locally

			for {
				if atomic.LoadInt32(&dequeuedCount) == numEnqueuers*numItems {
					break // Stop dequeuing when all enqueues are done, and the queue is empty
				}

				item, ok := queue.Dequeue()
				if ok {
					if debug {
						fmt.Printf("Dequeued item %v by id %d\n", item, id)
					}
					localDequeued[item.EnqueuerID] = append(localDequeued[item.EnqueuerID], item)
					if debug && len(localDequeued[item.EnqueuerID]) > 1 {
						fmt.Printf("Last Dequeued item %v by id %d\n", localDequeued[item.EnqueuerID][len(localDequeued[item.EnqueuerID])-2], id)
					}
					atomic.AddInt32(&dequeuedCount, 1)
				}
			}

			// Step 2: Local Validation (ensuring sorted order per enqueuer ID)
			for enqueuerID, values := range localDequeued {
				if len(values) == 0 {
					continue
				}
				if debug {
					for _, v := range values {
						fmt.Printf("Dequeued item %v by id %d\n", v, id)
					}
				}

				previous := values[0]
				for _, v := range values[1:] {
					if debug {
						fmt.Printf("comparing %v and %v\n", previous, v)
					}
					require.Greater(t, v.Value, previous.Value, "Dequeued items are not in sorted order for enqueuer %d: %d before %d", enqueuerID, previous, v)
					previous = v
				}
			}

			// Step 3: Store validated results globally
			mtx.Lock()
			for _, values := range localDequeued {
				for _, v := range values {
					if debug {
						if _, ok := globalDequeuedData[v]; ok {
							fmt.Printf("Duplicate value %v found in globalDequeuedData by id %d\n", v, i)
						}
					}
					globalDequeuedData[v] = struct{}{}
				}
			}
			mtx.Unlock()
		}(i)
	}

	// Wait until all dequeuers are ready
	for atomic.LoadInt32(&dequeuersReady) < numDequeuers {
	}

	// Step 4: Start enqueuers only after all dequeuers are ready
	for i := 0; i < numEnqueuers; i++ {
		wgEnqueue.Add(1)
		go func(id int) {
			atomic.AddInt32(&enqueuersReady, 1) // Mark this enqueuer as ready
			defer wgEnqueue.Done()

			// Wait until all enqueuers are ready before enqueueing
			for atomic.LoadInt32(&enqueuersReady) < numEnqueuers {
			}

			startValue := id * numItems // Each enqueuer starts from a different range (numItems, 2*numItems, ...)
			for j := 0; j < numItems; j++ {
				item := QueueItem{EnqueuerID: id, Value: startValue + j} // Unique sorted values per enqueuer
				queue.Enqueue(item)
				if debug {
					fmt.Printf("Enqueued item %v by id %d\n", item, id)
				}
				atomic.AddInt32(&enqueuedCount, 1)
			}
		}(i)
	}

	// Step 5: Wait for all enqueuers to finish
	wgEnqueue.Wait()
	atomic.StoreInt32(&allEnqFinished, 1)

	// Step 6: Wait for all dequeuers to finish
	wgDequeue.Wait()

	// Step 7: Full Validation - Ensure all items are present
	expectedCounts := make(map[QueueItem]struct{})
	for i := 0; i < numEnqueuers; i++ {
		for j := i * numItems; j < (i+1)*numItems; j++ {
			expectedCounts[QueueItem{EnqueuerID: i, Value: j}] = struct{}{}
		}
	}

	require.Equal(t, len(expectedCounts), len(globalDequeuedData), "Dequeued items count mismatch")
	for item := range expectedCounts {
		_, ok := globalDequeuedData[item]
		require.True(t, ok, "Dequeued item not found: %v", item)
	}

	// Step 8: Ensure queue is empty at the end
	require.True(t, queue.IsEmpty(), "Queue is not empty at the end of the test")
}

// BenchmarkMSQueueConcurrent benchmarks MSQueue with multiple enqueuers and dequeuers.
func BenchmarkMSQueueConcurrent(b *testing.B) {
	queue := NewMSQueue[QueueItem]() // Create an MSQueue of struct type

	var wgEnqueue sync.WaitGroup
	var wgDequeue sync.WaitGroup

	var enqueuersReady int32 = 0 // Atomic counter to track enqueuer readiness
	var dequeuersReady int32 = 0 // Atomic counter to track dequeuer readiness
	enqueuedCount := int32(0)    // Track the number of elements enqueued
	dequeuedCount := int32(0)    // Track the number of elements dequeued

	for i := 0; i < numDequeuers; i++ {
		wgDequeue.Add(1)
		go func() {
			atomic.AddInt32(&dequeuersReady, 1) // Mark this dequeuer as ready
			defer wgDequeue.Done()

			for atomic.LoadInt32(&dequeuedCount) < numEnqueuers*numItems {
				if _, ok := queue.Dequeue(); ok {
					atomic.AddInt32(&dequeuedCount, 1)
				}
			}
		}()
	}

	// Wait until all dequeuers are ready
	for atomic.LoadInt32(&dequeuersReady) < numDequeuers {
	}

	b.ResetTimer() // Start benchmarking from this point

	// Start enqueuers
	for i := 0; i < numEnqueuers; i++ {
		wgEnqueue.Add(1)
		go func(id int) {
			atomic.AddInt32(&enqueuersReady, 1) // Mark this enqueuer as ready
			defer wgEnqueue.Done()

			// Wait until all enqueuers are ready
			for atomic.LoadInt32(&enqueuersReady) < numEnqueuers {
			}

			startValue := id * numItems
			for j := 0; j < numItems; j++ {
				queue.Enqueue(QueueItem{EnqueuerID: id, Value: startValue + j})
				atomic.AddInt32(&enqueuedCount, 1)
			}
		}(i)
	}

	wgEnqueue.Wait()
	wgDequeue.Wait()

	b.StopTimer() // Stop the timer before ending the benchmark
}

package kumo_test

import (
	"testing"

	"github.com/1ntEgr8/kumo/implementations/prta_kumo"
	"github.com/1ntEgr8/kumo/implementations/prta_kumo_nonblocking"
	"github.com/1ntEgr8/kumo/implementations/prta_naive"
	"github.com/1ntEgr8/kumo/implementations/prta_nonblocking"
	"github.com/1ntEgr8/kumo/implementations/srta"
	"github.com/1ntEgr8/kumo/implementations/srta_kumo"
)

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

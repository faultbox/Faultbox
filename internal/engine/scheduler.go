package engine

import (
	"context"
	"fmt"
)

// ExploreScheduler controls the release ordering of held syscalls
// during parallel execution. It uses permutation indices to deterministically
// explore different interleavings.
type ExploreScheduler struct {
	// PermIndex selects which permutation of release order to use.
	// For exhaustive exploration, iterate 0..Factorial(N)-1.
	PermIndex int
}

// ReleaseInOrder waits for n items in the queue, then releases them
// in the order specified by the permutation derived from PermIndex.
// Returns the permutation used.
func (es *ExploreScheduler) ReleaseInOrder(ctx context.Context, q *HoldQueue, n int) ([]int, error) {
	// Wait until we have n held syscalls.
	if err := q.Wait(n, ctx); err != nil {
		return nil, fmt.Errorf("scheduler wait: %w", err)
	}

	perm := PermutationFromIndex(n, es.PermIndex)

	// Release in permutation order. After each release, indices shift,
	// so we track items by snapshot position.
	// Strategy: take snapshot, release by original position adjusted for shifts.
	for step := 0; step < n; step++ {
		targetOrigIdx := perm[step]

		// Compute the current index of the item originally at targetOrigIdx.
		// Items before it that have already been released shift it left.
		currentIdx := targetOrigIdx
		for _, prevReleased := range perm[:step] {
			if prevReleased < targetOrigIdx {
				currentIdx--
			}
		}

		if !q.ReleaseByIndex(currentIdx, HoldDecision{Allow: true}) {
			return perm, fmt.Errorf("scheduler: failed to release index %d (original %d, step %d)", currentIdx, targetOrigIdx, step)
		}
	}

	return perm, nil
}

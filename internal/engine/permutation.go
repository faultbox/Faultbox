package engine

// Factorial returns n! capped at maxN=12 to avoid overflow.
func Factorial(n int) int {
	if n <= 1 {
		return 1
	}
	if n > 12 {
		n = 12 // 12! = 479,001,600 — fits in int
	}
	result := 1
	for i := 2; i <= n; i++ {
		result *= i
	}
	return result
}

// PermutationFromIndex converts an index (0..n!-1) to a permutation of [0..n-1]
// using the factorial number system (Lehmer code).
func PermutationFromIndex(n, index int) []int {
	if n <= 0 {
		return nil
	}

	// Clamp index to valid range.
	total := Factorial(n)
	index = index % total

	// Build available elements.
	available := make([]int, n)
	for i := range available {
		available[i] = i
	}

	perm := make([]int, n)
	for i := 0; i < n; i++ {
		f := Factorial(n - 1 - i)
		digit := index / f
		index = index % f
		perm[i] = available[digit]
		available = append(available[:digit], available[digit+1:]...)
	}
	return perm
}

package engine

import (
	"fmt"
	"testing"
)

func TestFactorial(t *testing.T) {
	cases := []struct{ n, want int }{
		{0, 1}, {1, 1}, {2, 2}, {3, 6}, {4, 24}, {5, 120}, {6, 720},
	}
	for _, c := range cases {
		if got := Factorial(c.n); got != c.want {
			t.Errorf("Factorial(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

func TestPermutationFromIndex_N2(t *testing.T) {
	// 2! = 2 permutations: [0,1] and [1,0]
	expect := [][]int{{0, 1}, {1, 0}}
	for i, want := range expect {
		got := PermutationFromIndex(2, i)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("PermutationFromIndex(2, %d) = %v, want %v", i, got, want)
		}
	}
}

func TestPermutationFromIndex_N3(t *testing.T) {
	// 3! = 6 permutations
	expect := [][]int{
		{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0},
	}
	for i, want := range expect {
		got := PermutationFromIndex(3, i)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("PermutationFromIndex(3, %d) = %v, want %v", i, got, want)
		}
	}
}

func TestPermutationFromIndex_AllUnique(t *testing.T) {
	// Verify all 24 permutations of N=4 are unique.
	n := 4
	seen := make(map[string]bool)
	for i := 0; i < Factorial(n); i++ {
		p := PermutationFromIndex(n, i)
		key := fmt.Sprint(p)
		if seen[key] {
			t.Errorf("duplicate permutation at index %d: %v", i, p)
		}
		seen[key] = true
	}
	if len(seen) != 24 {
		t.Errorf("expected 24 unique permutations, got %d", len(seen))
	}
}

func TestPermutationFromIndex_Wraps(t *testing.T) {
	// Index beyond n! should wrap.
	p0 := PermutationFromIndex(3, 0)
	p6 := PermutationFromIndex(3, 6) // 6 % 6 = 0
	if fmt.Sprint(p0) != fmt.Sprint(p6) {
		t.Errorf("expected wrap: index 0 = %v, index 6 = %v", p0, p6)
	}
}

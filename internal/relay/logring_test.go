package relay

import (
	"fmt"
	"sync"
	"testing"
)

func TestLogRingOrderedOldestFirst(t *testing.T) {
	r := NewLogRing(4)
	r.Write([]byte("one\n"))
	r.Write([]byte("two\nthree\n")) // one Write may carry several lines
	got := r.Lines()
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Lines()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLogRingWrapsAtCapacity(t *testing.T) {
	r := NewLogRing(3)
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(r, "line-%d\n", i)
	}
	got := r.Lines()
	want := []string{"line-3", "line-4", "line-5"}
	if len(got) != len(want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Lines()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLogRingConcurrentWrites(t *testing.T) {
	r := NewLogRing(64)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				fmt.Fprintf(r, "g%d-%d\n", g, i)
			}
		}(g)
	}
	wg.Wait()
	if got := len(r.Lines()); got != 64 {
		t.Fatalf("len(Lines()) = %d, want full ring of 64", got)
	}
}

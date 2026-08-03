package clock

import (
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

func TestSystemClockAdvances(t *testing.T) {
	// Not a timing assertion — no sleep, no threshold. It only proves System
	// reads a real clock rather than returning a zero value, which is the one
	// way the real implementation could be silently wrong.
	if got := (System{}).Now(); got.IsZero() {
		t.Fatal("System.Now() returned the zero time")
	}
}

func TestFakeMovesOnlyWhenAdvanced(t *testing.T) {
	c := NewFake(epoch)

	if got := c.Now(); !got.Equal(epoch) {
		t.Fatalf("Now() = %v, want %v", got, epoch)
	}
	if got := c.Now(); !got.Equal(epoch) {
		t.Fatalf("Now() moved without Advance: %v", got)
	}

	want := epoch.Add(90 * time.Second)
	if got := c.Advance(90 * time.Second); !got.Equal(want) {
		t.Fatalf("Advance() = %v, want %v", got, want)
	}
	if got := c.Now(); !got.Equal(want) {
		t.Fatalf("Now() after Advance = %v, want %v", got, want)
	}
}

func TestElapsedMS(t *testing.T) {
	tests := []struct {
		name    string
		advance time.Duration
		want    int
	}{
		{name: "no time passed", advance: 0, want: 0},
		{name: "sub-millisecond truncates down", advance: 999 * time.Microsecond, want: 0},
		{name: "exactly one millisecond", advance: time.Millisecond, want: 1},
		{name: "truncates rather than rounds", advance: 1900 * time.Microsecond, want: 1},
		{name: "a whole second", advance: time.Second, want: 1000},
		{name: "past the wall-clock cap", advance: 31 * time.Second, want: 31000},
		// A monotonic clock will not go backwards, but a wall-clock read across
		// an NTP correction can. trace_event.v1 forbids a negative elapsed_ms,
		// so this must floor rather than negate.
		{name: "clock went backwards", advance: -5 * time.Second, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewFake(epoch)
			start := c.Now()
			c.Advance(tt.advance)

			if got := ElapsedMS(c, start); got != tt.want {
				t.Errorf("ElapsedMS() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFakeIsSafeUnderConcurrentUse(t *testing.T) {
	// The agent loop reads the clock on the goroutine running the loop while a
	// test advances it from the test goroutine. Without the mutex this is a
	// data race, and -race would report it as a failure in whatever test
	// happened to be running rather than here.
	c := NewFake(epoch)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); c.Advance(time.Millisecond) }()
		go func() { defer wg.Done(); _ = c.Now() }()
	}
	wg.Wait()

	if got, want := c.Now(), epoch.Add(8*time.Millisecond); !got.Equal(want) {
		t.Errorf("after 8 advances Now() = %v, want %v", got, want)
	}
}

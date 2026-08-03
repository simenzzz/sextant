// Package clock is the runtime's only source of time.
//
// Nothing outside this package calls time.Now. Every elapsed_ms on the wire,
// every wall-clock budget decision, and every duration in the cost ledger is
// read through a Clock, so a test can drive a thirty-second cap to its limit
// without waiting thirty seconds and without a sleep anywhere in the suite.
//
// The house rule this serves: no sleeps, no wall-clock, no flake. A test that
// sleeps is a test that is slow when it passes and wrong when the machine is
// loaded.
package clock

import (
	"sync"
	"time"
)

// Clock reads the current time.
//
// Deliberately one method. A wider interface — timers, tickers, After — would
// have to be faked correctly by every test double, and the runtime does not
// need them: bounded waiting is context's job, not the clock's.
type Clock interface {
	Now() time.Time
}

// System is the real clock.
type System struct{}

var _ Clock = System{}

// Now returns the current time.
func (System) Now() time.Time { return time.Now() }

// Fake is a Clock a test drives by hand.
//
// Safe for concurrent use: the agent loop reads the clock from the goroutine
// running the loop while a test advances it from the test goroutine, and
// without the mutex that is a data race the race detector would report as a
// failure in whatever test happened to be running.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

var _ Clock = (*Fake)(nil)

// NewFake returns a Fake reading start.
//
// A zero start would make elapsed-time arithmetic accidentally correct even
// when the code under test forgot to record a beginning, so callers should
// pass a real-looking instant.
func NewFake(start time.Time) *Fake {
	return &Fake{now: start}
}

// Now returns the fake's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the fake forward by d and returns the new time.
//
// Moving backwards is allowed and is worth testing: a real monotonic clock
// will not do it, but a wall-clock read across an NTP correction can, and code
// that computes an elapsed duration by subtraction should not produce a
// negative value on the wire — trace_event.v1 forbids one.
func (f *Fake) Advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}

// ElapsedMS returns the whole milliseconds between start and the clock's
// current time, floored at zero.
//
// Floored, not absolute: a clock that went backwards means the elapsed time is
// unknown, and reporting zero says "no measurable time passed" where a
// negated duration would claim the step ran before it started. Both trace
// events and ledger entries require a non-negative integer, so this is the one
// place that conversion happens.
func ElapsedMS(c Clock, start time.Time) int {
	d := c.Now().Sub(start)
	if d <= 0 {
		return 0
	}
	return int(d.Milliseconds())
}

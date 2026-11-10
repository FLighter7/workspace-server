package pairing

import (
	"sync"
	"time"
)

const (
	maxRefusals = 5
	refusalTTL  = 10 * time.Minute
)

// Refusal is a pairing attempt with a valid secret for an app the server does
// not serve.
type Refusal struct {
	App, Device string
	At          time.Time
}

// Refusals remembers the last few refused pairings for a short time.
type Refusals struct {
	now func() time.Time

	mu      sync.Mutex
	entries []Refusal // oldest first
}

// NewRefusals returns an empty Refusals that reads time from now.
func NewRefusals(now func() time.Time) *Refusals {
	return &Refusals{now: now}
}

// Record remembers that device tried to pair app. A repeat of the same app and
// device name replaces the earlier entry, so one device that keeps retrying
// takes one place, the newest.
func (r *Refusals) Record(app, device string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	kept := r.entries[:0]
	for _, e := range r.entries {
		if e.App != app || e.Device != device {
			kept = append(kept, e)
		}
	}
	r.entries = append(kept, Refusal{App: app, Device: device, At: now})
	r.pruneLocked(now)
}

// Recent returns the remembered refusals, newest first: at most 5 distinct
// (app, device name) pairs, none older than 10 minutes. An entry exactly 10
// minutes old is still listed.
func (r *Refusals) Recent() []Refusal {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.now())
	out := make([]Refusal, len(r.entries))
	for i, e := range r.entries {
		out[len(out)-1-i] = e
	}
	return out
}

// Clear forgets every refusal for app.
func (r *Refusals) Clear(app string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.entries[:0]
	for _, e := range r.entries {
		if e.App != app {
			kept = append(kept, e)
		}
	}
	r.entries = kept
}

// pruneLocked drops entries older than the TTL and all but the newest five.
func (r *Refusals) pruneLocked(now time.Time) {
	cutoff := now.Add(-refusalTTL)
	kept := r.entries[:0]
	for _, e := range r.entries {
		if !e.At.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	if len(kept) > maxRefusals {
		kept = append(kept[:0], kept[len(kept)-maxRefusals:]...)
	}
	r.entries = kept
}

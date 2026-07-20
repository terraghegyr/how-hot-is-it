package main

import (
	"fmt"
	"time"
)

// Alerting tunables. All hardcoded per the design (retention/hysteresis/re-notify
// windows are deliberately not configurable).
const (
	staleAfter    = 10 * time.Minute // silence before a machine is "stale"
	renotifyEvery = 30 * time.Minute // repeat interval while still breaching
	hysteresisC   = 3.0              // recover only below threshold-hysteresis
)

// EventType is the kind of alert-state transition.
type EventType string

const (
	EventBreach        EventType = "breach"
	EventRecovery      EventType = "recovery"
	EventStale         EventType = "stale"
	EventStaleRecovery EventType = "stale_recovery"
	EventAggregated    EventType = "aggregated"
)

// AlertState is the persisted per-machine state driving the alert machine.
type AlertState struct {
	Alerting      bool
	LastNotified  int64 // unix seconds of last breach notification (for re-notify)
	StaleNotified bool
	AggNotified   bool // aggregated-alert de-dup; no recovery message, reset silently
}

// Reading is the latest reading for a machine. HasData is false when the machine
// has never reported.
type Reading struct {
	TempC   float64
	TS      int64 // unix seconds, server-assigned
	HasData bool
}

// Event is one transition emitted by Evaluate. Persist marks whether it should be
// written to the alerts history table; re-notifications set Persist=false (they
// are still delivered to Telegram but do not create a new history row).
type Event struct {
	Type    EventType
	TempC   *float64 // nil for stale/stale_recovery
	Persist bool
	Count   int // number of readings in window, for EventAggregated only
}

// Message renders the human-facing text for an event given the machine name and
// the configured threshold. Kept pure so it is trivially testable.
func (e Event) Message(name string, threshold float64) string {
	switch e.Type {
	case EventBreach:
		return fmt.Sprintf("🔥 %s: %.1f°C (threshold %.0f°C)", name, deref(e.TempC), threshold)
	case EventRecovery:
		return fmt.Sprintf("✅ %s back to %.1f°C", name, deref(e.TempC))
	case EventStale:
		return fmt.Sprintf("⚠️ %s stopped reporting", name)
	case EventStaleRecovery:
		return fmt.Sprintf("📡 %s reporting again", name)
	default:
		return name
	}
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func tempPtr(f float64) *float64 { return &f }

// Evaluate is the pure alert state machine. Given the current state, the latest
// reading, and the current time, it returns the new state plus the transitions to
// act on. It never touches HTTP, SQLite, or the wall clock.
func Evaluate(st AlertState, r Reading, now time.Time, threshold float64) (AlertState, []Event) {
	var events []Event

	// Stale handling takes precedence: a machine that has reported before but has
	// gone silent for staleAfter is flagged once. While stale we do not evaluate
	// the (old) temperature against the threshold.
	if r.HasData && now.Unix()-r.TS >= int64(staleAfter.Seconds()) {
		if !st.StaleNotified {
			st.StaleNotified = true
			events = append(events, Event{Type: EventStale, Persist: true})
		}
		return st, events
	}

	// Fresh (or never-seen) data below the stale window. If we were stale, the
	// machine has come back.
	if st.StaleNotified {
		st.StaleNotified = false
		events = append(events, Event{Type: EventStaleRecovery, Persist: true})
	}

	if !r.HasData {
		return st, events
	}

	switch {
	case r.TempC >= threshold:
		if !st.Alerting {
			st.Alerting = true
			st.LastNotified = now.Unix()
			events = append(events, Event{Type: EventBreach, TempC: tempPtr(r.TempC), Persist: true})
		} else if now.Unix()-st.LastNotified >= int64(renotifyEvery.Seconds()) {
			// Sustained breach: re-notify, but do not create a new history row.
			st.LastNotified = now.Unix()
			events = append(events, Event{Type: EventBreach, TempC: tempPtr(r.TempC), Persist: false})
		}
	case r.TempC < threshold-hysteresisC:
		if st.Alerting {
			st.Alerting = false
			st.LastNotified = 0
			events = append(events, Event{Type: EventRecovery, TempC: tempPtr(r.TempC), Persist: true})
		}
	default:
		// Inside the hysteresis band [threshold-hysteresis, threshold): hold state,
		// emit nothing. This is what prevents flapping.
	}

	return st, events
}

// EvaluateAggregated is the pure aggregated-alert machine. count is the number of
// readings in the window at/above the aggregated threshold; maxTemp is the hottest
// of them (may be nil). It fires once when count exceeds thresholdCount, unless a
// main breach is already active (st.Alerting) — that case is treated as consumed
// so the aggregated alert doesn't fire the instant the main breach recovers while
// the window is still warm. There is no recovery event: the flag resets silently
// once the window count falls back to the threshold or below.
func EvaluateAggregated(st AlertState, count, thresholdCount int, maxTemp *float64) (AlertState, []Event) {
	over := count > thresholdCount
	switch {
	case !over:
		st.AggNotified = false
		return st, nil
	case st.Alerting:
		// Covered by the active main breach; mark consumed, don't fire.
		st.AggNotified = true
		return st, nil
	default: // over && main not alerting
		if st.AggNotified {
			return st, nil
		}
		st.AggNotified = true
		return st, []Event{{Type: EventAggregated, TempC: maxTemp, Persist: true, Count: count}}
	}
}

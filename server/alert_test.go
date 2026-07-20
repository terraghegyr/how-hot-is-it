package main

import (
	"testing"
	"time"
)

const testThreshold = 80.0

func at(base time.Time, d time.Duration) time.Time { return base.Add(d) }

func reading(temp float64, ts time.Time) Reading {
	return Reading{TempC: temp, TS: ts.Unix(), HasData: true}
}

func countPersisted(events []Event) int {
	n := 0
	for _, e := range events {
		if e.Persist {
			n++
		}
	}
	return n
}

func TestBreachFiresOnce(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	st := AlertState{}

	st, ev := Evaluate(st, reading(85, base), base, testThreshold)
	if len(ev) != 1 || ev[0].Type != EventBreach || !ev[0].Persist {
		t.Fatalf("expected one persisted breach, got %+v", ev)
	}
	if !st.Alerting {
		t.Fatal("expected alerting=true after breach")
	}

	// Second evaluation shortly after: still breaching but no new event.
	st2, ev2 := Evaluate(st, reading(86, at(base, time.Minute)), at(base, time.Minute), testThreshold)
	if len(ev2) != 0 {
		t.Fatalf("expected no event on sustained breach before re-notify, got %+v", ev2)
	}
	if !st2.Alerting {
		t.Fatal("expected still alerting")
	}
}

func TestReNotifyExactlyAt30Min(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	st, _ := Evaluate(AlertState{}, reading(85, base), base, testThreshold)

	// 29 minutes: no re-notify.
	_, ev := Evaluate(st, reading(85, at(base, 29*time.Minute)), at(base, 29*time.Minute), testThreshold)
	if len(ev) != 0 {
		t.Fatalf("expected no re-notify at 29min, got %+v", ev)
	}

	// Exactly 30 minutes: re-notify, but NOT persisted (no new history row).
	st30, ev30 := Evaluate(st, reading(85, at(base, 30*time.Minute)), at(base, 30*time.Minute), testThreshold)
	if len(ev30) != 1 || ev30[0].Type != EventBreach {
		t.Fatalf("expected re-notify breach at 30min, got %+v", ev30)
	}
	if ev30[0].Persist {
		t.Fatal("re-notify must not be persisted to alert history")
	}
	if st30.LastNotified != at(base, 30*time.Minute).Unix() {
		t.Fatal("re-notify should reset LastNotified")
	}
}

func TestRecoveryOnlyBelowHysteresis(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	st, _ := Evaluate(AlertState{}, reading(85, base), base, testThreshold)

	// threshold-2 (78) is inside the hysteresis band: no recovery.
	st1, ev := Evaluate(st, reading(78, at(base, time.Minute)), at(base, time.Minute), testThreshold)
	if len(ev) != 0 || !st1.Alerting {
		t.Fatalf("expected no recovery inside band, got %+v alerting=%v", ev, st1.Alerting)
	}

	// threshold-4 (76) is below threshold-3: recovery fires once, persisted.
	st2, ev2 := Evaluate(st1, reading(76, at(base, 2*time.Minute)), at(base, 2*time.Minute), testThreshold)
	if len(ev2) != 1 || ev2[0].Type != EventRecovery || !ev2[0].Persist {
		t.Fatalf("expected persisted recovery, got %+v", ev2)
	}
	if st2.Alerting {
		t.Fatal("expected alerting=false after recovery")
	}
}

func TestFlappingInBandNoEvents(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	st := AlertState{}
	// Oscillate within [threshold-3, threshold): 78, 79, 77.1, 79 ...
	temps := []float64{78, 79, 77.5, 79.9, 78.2}
	for i, tmp := range temps {
		ts := at(base, time.Duration(i)*time.Minute)
		var ev []Event
		st, ev = Evaluate(st, reading(tmp, ts), ts, testThreshold)
		if len(ev) != 0 {
			t.Fatalf("temp %.1f produced events %+v (should be silent)", tmp, ev)
		}
		if st.Alerting {
			t.Fatalf("temp %.1f should not trigger alerting", tmp)
		}
	}
}

func TestStaleFiresOnceThenRecovers(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	st := AlertState{}

	// Last reading is 11 minutes old -> stale.
	staleReading := reading(50, at(base, -11*time.Minute))
	st, ev := Evaluate(st, staleReading, base, testThreshold)
	if len(ev) != 1 || ev[0].Type != EventStale || !ev[0].Persist {
		t.Fatalf("expected one persisted stale event, got %+v", ev)
	}
	if ev[0].TempC != nil {
		t.Fatal("stale event should carry no temperature")
	}

	// Still stale next tick: no repeat.
	st2, ev2 := Evaluate(st, reading(50, at(base, -12*time.Minute)), at(base, time.Minute), testThreshold)
	if len(ev2) != 0 {
		t.Fatalf("stale should not re-fire, got %+v", ev2)
	}

	// Fresh reading arrives -> stale_recovery.
	st3, ev3 := Evaluate(st2, reading(55, at(base, 2*time.Minute)), at(base, 2*time.Minute), testThreshold)
	if len(ev3) != 1 || ev3[0].Type != EventStaleRecovery || !ev3[0].Persist {
		t.Fatalf("expected persisted stale_recovery, got %+v", ev3)
	}
	if st3.StaleNotified {
		t.Fatal("StaleNotified should reset after recovery")
	}
}

func TestStaleRecoveryPlusBreachSameTick(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	// Start stale.
	st, _ := Evaluate(AlertState{}, reading(50, at(base, -11*time.Minute)), base, testThreshold)
	// Comes back hot in one tick: both stale_recovery and breach.
	_, ev := Evaluate(st, reading(90, at(base, time.Minute)), at(base, time.Minute), testThreshold)
	if len(ev) != 2 {
		t.Fatalf("expected stale_recovery + breach, got %+v", ev)
	}
	if ev[0].Type != EventStaleRecovery || ev[1].Type != EventBreach {
		t.Fatalf("unexpected event order/types: %+v", ev)
	}
	if countPersisted(ev) != 2 {
		t.Fatal("both transitions should persist")
	}
}

func TestNeverReportedNoEvents(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	_, ev := Evaluate(AlertState{}, Reading{HasData: false}, base, testThreshold)
	if len(ev) != 0 {
		t.Fatalf("machine with no data should produce no events, got %+v", ev)
	}
}

const testAggCount = 5

func TestAggregatedBelowCountNoEvent(t *testing.T) {
	st, ev := EvaluateAggregated(AlertState{}, 5, testAggCount, tempPtr(55)) // 5 is not > 5
	if len(ev) != 0 {
		t.Fatalf("count == threshold should not fire, got %+v", ev)
	}
	if st.AggNotified {
		t.Fatal("flag should stay false below threshold")
	}
}

func TestAggregatedFiresOnce(t *testing.T) {
	st, ev := EvaluateAggregated(AlertState{}, 6, testAggCount, tempPtr(57))
	if len(ev) != 1 || ev[0].Type != EventAggregated || !ev[0].Persist {
		t.Fatalf("expected one persisted aggregated event, got %+v", ev)
	}
	if ev[0].Count != 6 || ev[0].TempC == nil || *ev[0].TempC != 57 {
		t.Fatalf("event should carry count and max temp, got %+v", ev[0])
	}
	if !st.AggNotified {
		t.Fatal("flag should be set after firing")
	}
	// Still over, already notified -> no re-fire.
	_, ev2 := EvaluateAggregated(st, 8, testAggCount, tempPtr(60))
	if len(ev2) != 0 {
		t.Fatalf("should not re-fire while still notified, got %+v", ev2)
	}
}

func TestAggregatedResetsThenCanFireAgain(t *testing.T) {
	st, _ := EvaluateAggregated(AlertState{}, 6, testAggCount, tempPtr(55))
	// Count falls back to/below threshold -> flag resets silently, no event.
	st, ev := EvaluateAggregated(st, 3, testAggCount, nil)
	if len(ev) != 0 || st.AggNotified {
		t.Fatalf("expected silent reset, got events=%+v notified=%v", ev, st.AggNotified)
	}
	// Rises again -> fires again.
	_, ev = EvaluateAggregated(st, 7, testAggCount, tempPtr(56))
	if len(ev) != 1 {
		t.Fatalf("expected re-fire after reset, got %+v", ev)
	}
}

func TestAggregatedSuppressedByMainBreach(t *testing.T) {
	// Main breach active: aggregated must not fire even though count is over.
	st, ev := EvaluateAggregated(AlertState{Alerting: true}, 9, testAggCount, tempPtr(85))
	if len(ev) != 0 {
		t.Fatalf("aggregated must be suppressed while main breach active, got %+v", ev)
	}
	if !st.AggNotified {
		t.Fatal("suppression should mark the flag consumed")
	}
	// Main recovers (Alerting=false) but window still warm (count over) and flag
	// consumed -> still no fire (no duplicate alert on recovery).
	st.Alerting = false
	_, ev = EvaluateAggregated(st, 9, testAggCount, tempPtr(70))
	if len(ev) != 0 {
		t.Fatalf("no aggregated alert should fire right after a main recovery, got %+v", ev)
	}
}

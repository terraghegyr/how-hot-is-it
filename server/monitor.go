package main

import (
	"fmt"
	"log"
	"sync"
	"time"
)

const readingRetention = 24 * time.Hour

// Monitor runs alert evaluation. Evaluation happens both on every reading ingest
// (so a sub-minute spike is never missed) and on a periodic maintenance tick
// (which also prunes readings, drives stale/re-notify timing, and caps the alerts
// table). The clock is injectable so tests never sleep.
type Monitor struct {
	store           *Store
	notify          Notifier
	threshold       float64
	alertingEnabled bool
	now             func() time.Time
	mu              sync.Mutex // serializes EvaluateMachine (ingest vs tick)

	// Aggregated alert (all zero/false = disabled).
	aggEnabled   bool
	aggThreshold float64
	aggCount     int
	aggWindow    time.Duration
}

// message renders the Telegram/history text for an event. Aggregated events need
// config context (count, window) that the pure Event.Message doesn't carry.
func (m *Monitor) message(ev Event, name string) string {
	if ev.Type == EventAggregated {
		return fmt.Sprintf("📊 %s: %d readings ≥ %.0f°C in %d min", name, ev.Count, m.aggThreshold, int(m.aggWindow.Minutes()))
	}
	return ev.Message(name, m.threshold)
}

// EvaluateMachine runs the alert state machine for one machine against its latest
// reading, delivers any notifications, records transitions, and persists the new
// state. It is safe to call concurrently from the ingest path and the tick.
func (m *Monitor) EvaluateMachine(machineID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	st, err := m.store.GetAlertState(machineID)
	if err != nil {
		return err
	}
	r, err := m.store.LatestReading(machineID)
	if err != nil {
		return err
	}
	newSt, events := Evaluate(st, r, now, m.threshold)

	// Aggregated alert: evaluated against the count of recent readings above the
	// aggregated threshold, using the just-updated main Alerting state so an
	// active breach suppresses it.
	if m.aggEnabled {
		since := now.Add(-m.aggWindow).Unix()
		count, maxT, err := m.store.CountReadingsAbove(machineID, since, m.aggThreshold)
		if err != nil {
			return err
		}
		var aggEvents []Event
		newSt, aggEvents = EvaluateAggregated(newSt, count, m.aggCount, maxT)
		events = append(events, aggEvents...)
	}

	for _, ev := range events {
		ok := true
		if m.alertingEnabled {
			ok = m.notify.Send(m.message(ev, name))
		}
		if ev.Persist {
			if err := m.store.InsertAlert(AlertRow{
				MachineID:   machineID,
				MachineName: name,
				TS:          now.Unix(),
				Type:        string(ev.Type),
				TempC:       ev.TempC,
				TelegramOK:  ok,
			}); err != nil {
				return err
			}
		}
	}
	return m.store.SaveAlertState(machineID, newSt)
}

// Tick performs one maintenance cycle: prune old readings, evaluate every machine
// (covers stale detection and re-notify timing, which the ingest path can't), and
// cap the alerts table.
func (m *Monitor) Tick() error {
	if err := m.store.PruneReadings(m.now().Add(-readingRetention).Unix()); err != nil {
		return err
	}
	machines, err := m.store.ListMachines()
	if err != nil {
		return err
	}
	for _, mc := range machines {
		if err := m.EvaluateMachine(mc.ID, mc.Name); err != nil {
			return err
		}
	}
	return m.store.CapAlerts()
}

// Run ticks every interval until the stop channel is closed.
func (m *Monitor) Run(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := m.Tick(); err != nil {
				log.Printf("maintenance tick error: %v", err)
			}
		}
	}
}

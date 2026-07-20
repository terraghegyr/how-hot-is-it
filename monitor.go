package main

import (
	"log"
	"time"
)

const readingRetention = 24 * time.Hour

// Monitor runs the periodic maintenance tick: prune old readings, evaluate the
// alert state machine for every machine, deliver notifications, record history,
// and cap the alerts table. The clock is injectable so tests never sleep.
type Monitor struct {
	store           *Store
	notify          Notifier
	threshold       float64
	alertingEnabled bool
	now             func() time.Time
}

// Tick performs one maintenance cycle.
func (m *Monitor) Tick() error {
	now := m.now()

	if err := m.store.PruneReadings(now.Add(-readingRetention).Unix()); err != nil {
		return err
	}

	machines, err := m.store.ListMachines()
	if err != nil {
		return err
	}
	for _, mc := range machines {
		st, err := m.store.GetAlertState(mc.ID)
		if err != nil {
			return err
		}
		r, err := m.store.LatestReading(mc.ID)
		if err != nil {
			return err
		}
		newSt, events := Evaluate(st, r, now, m.threshold)
		for _, ev := range events {
			ok := true
			if m.alertingEnabled {
				ok = m.notify.Send(ev.Message(mc.Name, m.threshold))
			}
			if ev.Persist {
				if err := m.store.InsertAlert(AlertRow{
					MachineID:   mc.ID,
					MachineName: mc.Name,
					TS:          now.Unix(),
					Type:        string(ev.Type),
					TempC:       ev.TempC,
					TelegramOK:  ok,
				}); err != nil {
					return err
				}
			}
		}
		if err := m.store.SaveAlertState(mc.ID, newSt); err != nil {
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

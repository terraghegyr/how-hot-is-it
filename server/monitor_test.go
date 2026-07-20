package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is an advanceable clock for driving the monitor without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// A breach reading must fire an alert on ingest, without waiting for a tick —
// this is what prevents sub-minute spikes between ticks from being missed.
func TestIngestTriggersImmediateAlert(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.CreateMachine("hotbox", 1000)

	var msgCount int32
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&msgCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer tg.Close()

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	notifier := &TelegramNotifier{BaseURL: tg.URL, Token: "t", ChatID: "c", Client: &http.Client{Timeout: 5 * time.Second}}
	mon := &Monitor{store: s, notify: notifier, threshold: testThreshold, alertingEnabled: true, now: clk.now}

	// Wire the report endpoint to evaluate synchronously (deterministic in tests).
	srv := newTestServerHook(t, s, clk.now, func(id, name string) { mon.EvaluateMachine(id, name) })

	resp, err := http.Post(srv.URL+"/api/report", "application/json",
		strings.NewReader(`{"machine_id":"`+m.ID+`","temp_c":90}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("report status %d", resp.StatusCode)
	}

	// No tick was run — the alert must have fired from the ingest path alone.
	if got := atomic.LoadInt32(&msgCount); got != 1 {
		t.Fatalf("expected 1 immediate alert on ingest, got %d", got)
	}
	alerts, _ := s.ListAlerts(50)
	if len(alerts) != 1 || alerts[0].Type != "breach" {
		t.Fatalf("expected one breach row, got %+v", alerts)
	}
}

// Report readings that stay below the main threshold but above the aggregated
// one; once more than aggCount land in the window, exactly one aggregated alert
// fires via the ingest path.
func TestIngestAggregatedAlert(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.CreateMachine("warmbox", 1000)

	var msgCount int32
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&msgCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer tg.Close()

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	notifier := &TelegramNotifier{BaseURL: tg.URL, Token: "t", ChatID: "c", Client: &http.Client{Timeout: 5 * time.Second}}
	mon := &Monitor{
		store: s, notify: notifier, threshold: testThreshold, alertingEnabled: true, now: clk.now,
		aggEnabled: true, aggThreshold: 50, aggCount: 5, aggWindow: 15 * time.Minute,
	}
	srv := newTestServerHook(t, s, clk.now, func(id, name string) { mon.EvaluateMachine(id, name) })

	post := func(temp string) {
		resp, err := http.Post(srv.URL+"/api/report", "application/json",
			strings.NewReader(`{"machine_id":"`+m.ID+`","temp_c":`+temp+`}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// 5 warm readings (55°C): at/above 50 but count is not yet > 5.
	for i := 0; i < 5; i++ {
		post("55")
	}
	if got := atomic.LoadInt32(&msgCount); got != 0 {
		t.Fatalf("no alert expected at count 5, got %d", got)
	}
	// 6th crosses the count -> one aggregated alert.
	post("55")
	if got := atomic.LoadInt32(&msgCount); got != 1 {
		t.Fatalf("expected exactly 1 aggregated alert, got %d", got)
	}
	// Further warm readings do not re-fire.
	post("55")
	if got := atomic.LoadInt32(&msgCount); got != 1 {
		t.Fatalf("aggregated must not re-fire, got %d", got)
	}
	alerts, _ := s.ListAlerts(50)
	if len(alerts) != 1 || alerts[0].Type != "aggregated" {
		t.Fatalf("expected one aggregated row, got %+v", alerts)
	}
	if alerts[0].TempC == nil || *alerts[0].TempC != 55 {
		t.Fatalf("aggregated row should carry the window max temp 55, got %v", alerts[0].TempC)
	}
}

// A main breach suppresses the aggregated alert even though the window is full of
// hot readings.
func TestIngestAggregatedSuppressedByBreach(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.CreateMachine("hotbox", 1000)

	var msgCount int32
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&msgCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer tg.Close()

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	notifier := &TelegramNotifier{BaseURL: tg.URL, Token: "t", ChatID: "c", Client: &http.Client{Timeout: 5 * time.Second}}
	mon := &Monitor{
		store: s, notify: notifier, threshold: testThreshold, alertingEnabled: true, now: clk.now,
		aggEnabled: true, aggThreshold: 50, aggCount: 5, aggWindow: 15 * time.Minute,
	}
	srv := newTestServerHook(t, s, clk.now, func(id, name string) { mon.EvaluateMachine(id, name) })

	// 8 readings well above the main threshold: the first fires a breach; the rest
	// grow the aggregated window count, but aggregated stays suppressed.
	for i := 0; i < 8; i++ {
		resp, err := http.Post(srv.URL+"/api/report", "application/json",
			strings.NewReader(`{"machine_id":"`+m.ID+`","temp_c":90}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if got := atomic.LoadInt32(&msgCount); got != 1 {
		t.Fatalf("expected only the breach message (aggregated suppressed), got %d", got)
	}
	alerts, _ := s.ListAlerts(50)
	if len(alerts) != 1 || alerts[0].Type != "breach" {
		t.Fatalf("expected only a breach row, got %+v", alerts)
	}
}

func TestIntegrationBreachSustainedRecovery(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.CreateMachine("hotbox", 1000)

	var msgCount int32
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&msgCount, 1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer tg.Close()

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	notifier := &TelegramNotifier{BaseURL: tg.URL, Token: "t", ChatID: "c", Client: &http.Client{Timeout: 5 * time.Second}}
	mon := &Monitor{store: s, notify: notifier, threshold: testThreshold, alertingEnabled: true, now: clk.now}

	// t0: breach.
	s.AddReading(m.ID, clk.now().Unix(), 85)
	if err := mon.Tick(); err != nil {
		t.Fatal(err)
	}
	// +30min: sustained -> one re-notify.
	clk.advance(30 * time.Minute)
	s.AddReading(m.ID, clk.now().Unix(), 85)
	if err := mon.Tick(); err != nil {
		t.Fatal(err)
	}
	// +1min: still hot but not yet 30min since last notify -> silent.
	clk.advance(1 * time.Minute)
	s.AddReading(m.ID, clk.now().Unix(), 85)
	if err := mon.Tick(); err != nil {
		t.Fatal(err)
	}
	// +29min (=60min total): recovery.
	clk.advance(29 * time.Minute)
	s.AddReading(m.ID, clk.now().Unix(), 70)
	if err := mon.Tick(); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt32(&msgCount); got != 3 {
		t.Fatalf("expected exactly 3 Telegram messages (breach, re-notify, recovery), got %d", got)
	}

	// History rows: only transitions persist (breach + recovery), not the re-notify.
	alerts, _ := s.ListAlerts(50)
	if len(alerts) != 2 {
		t.Fatalf("expected 2 alert history rows, got %d: %+v", len(alerts), alerts)
	}
	if alerts[0].Type != "recovery" || alerts[1].Type != "breach" {
		t.Fatalf("expected [recovery, breach] newest-first, got %s,%s", alerts[0].Type, alerts[1].Type)
	}
	for _, a := range alerts {
		if !a.TelegramOK {
			t.Fatalf("expected telegram_ok for %s", a.Type)
		}
	}
}

func TestIntegrationTelegramFailureRecorded(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.CreateMachine("hotbox", 1000)

	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer tg.Close()

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	notifier := &TelegramNotifier{BaseURL: tg.URL, Token: "t", ChatID: "c", Client: &http.Client{Timeout: 5 * time.Second}}
	mon := &Monitor{store: s, notify: notifier, threshold: testThreshold, alertingEnabled: true, now: clk.now}

	s.AddReading(m.ID, clk.now().Unix(), 90)
	if err := mon.Tick(); err != nil {
		t.Fatalf("tick must not fail on Telegram error: %v", err)
	}

	alerts, _ := s.ListAlerts(50)
	if len(alerts) != 1 || alerts[0].Type != "breach" {
		t.Fatalf("expected one breach row, got %+v", alerts)
	}
	if alerts[0].TelegramOK {
		t.Fatal("expected telegram_ok=false after 500 response")
	}
}

func TestIntegrationAlertingDisabledStillRecords(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.CreateMachine("hotbox", 1000)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	// alertingEnabled=false: no Telegram, but history must still be written with
	// telegram_ok=1 (delivery not attempted counts as ok).
	mon := &Monitor{store: s, notify: nil, threshold: testThreshold, alertingEnabled: false, now: clk.now}

	s.AddReading(m.ID, clk.now().Unix(), 95)
	if err := mon.Tick(); err != nil {
		t.Fatal(err)
	}
	alerts, _ := s.ListAlerts(50)
	if len(alerts) != 1 || alerts[0].Type != "breach" || !alerts[0].TelegramOK {
		t.Fatalf("expected one breach row with telegram_ok=true, got %+v", alerts)
	}
}

func TestIntegrationStaleDetection(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.CreateMachine("quietbox", 1000)

	var msgCount int32
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&msgCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer tg.Close()

	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	notifier := &TelegramNotifier{BaseURL: tg.URL, Token: "t", ChatID: "c", Client: &http.Client{Timeout: 5 * time.Second}}
	mon := &Monitor{store: s, notify: notifier, threshold: testThreshold, alertingEnabled: true, now: clk.now}

	// One reading, then silence past the 10-minute stale window.
	s.AddReading(m.ID, clk.now().Unix(), 40)
	mon.Tick() // healthy, no message
	clk.advance(11 * time.Minute)
	mon.Tick() // stale -> one message
	clk.advance(1 * time.Minute)
	mon.Tick() // still stale -> no repeat

	if got := atomic.LoadInt32(&msgCount); got != 1 {
		t.Fatalf("expected exactly 1 stale message, got %d", got)
	}

	// Reports again -> stale_recovery message.
	clk.advance(1 * time.Minute)
	s.AddReading(m.ID, clk.now().Unix(), 41)
	mon.Tick()
	if got := atomic.LoadInt32(&msgCount); got != 2 {
		t.Fatalf("expected stale + stale_recovery = 2 messages, got %d", got)
	}
}

package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	// Temp-file DB per test keeps tests fully isolated (a shared in-memory cache
	// would leak state across tests in the same process).
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func countRows(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", q, err)
	}
	return n
}

func TestPruneReadings(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.CreateMachine("box", 1000)
	now := int64(1_700_000_000)
	day := int64(86400)

	if err := s.AddReading(m.ID, now, 40); err != nil { // fresh
		t.Fatal(err)
	}
	if err := s.AddReading(m.ID, now-day-100, 41); err != nil { // >24h old
		t.Fatal(err)
	}
	if err := s.PruneReadings(now - day); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM readings`); got != 1 {
		t.Fatalf("expected 1 reading kept, got %d", got)
	}
	var keptTS int64
	s.db.QueryRow(`SELECT ts FROM readings`).Scan(&keptTS)
	if keptTS != now {
		t.Fatalf("wrong reading kept: %d", keptTS)
	}
}

func TestAlertsCappedAt500(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 600; i++ {
		if err := s.InsertAlert(AlertRow{MachineID: "x", MachineName: "x", TS: int64(i), Type: "breach", TelegramOK: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CapAlerts(); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM alerts`); got != maxAlertRows {
		t.Fatalf("expected %d alerts, got %d", maxAlertRows, got)
	}
	// The kept rows must be the newest 500 (ts 100..599).
	var minTS int64
	s.db.QueryRow(`SELECT MIN(ts) FROM alerts`).Scan(&minTS)
	if minTS != 100 {
		t.Fatalf("expected oldest kept ts=100, got %d", minTS)
	}
}

func TestDeleteMachineKeepsAlerts(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.CreateMachine("server-a", 1000)
	s.AddReading(m.ID, 2000, 70)
	s.SaveAlertState(m.ID, AlertState{Alerting: true, LastNotified: 2000})
	s.InsertAlert(AlertRow{MachineID: m.ID, MachineName: m.Name, TS: 2000, Type: "breach", TempC: tempPtr(85), TelegramOK: true})

	ok, err := s.DeleteMachine(m.ID)
	if err != nil || !ok {
		t.Fatalf("delete failed: ok=%v err=%v", ok, err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM machines`); got != 0 {
		t.Fatalf("machine not deleted: %d", got)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM readings`); got != 0 {
		t.Fatalf("readings not deleted: %d", got)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM alert_state`); got != 0 {
		t.Fatalf("alert_state not deleted: %d", got)
	}
	// Alerts row survives, still carrying the denormalized name.
	alerts, err := s.ListAlerts(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].MachineName != "server-a" {
		t.Fatalf("expected 1 surviving alert named server-a, got %+v", alerts)
	}

	// Deleting a non-existent machine returns false.
	ok, err = s.DeleteMachine("deadbeef")
	if err != nil || ok {
		t.Fatalf("expected false for missing machine, got ok=%v err=%v", ok, err)
	}
}

func TestHistoryColumnarWithGaps(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.CreateMachine("a", 1000)
	b, _ := s.CreateMachine("b", 1000)

	// Aligned minute buckets 0, 60, 120, 180. Machine b skips bucket 120 (outage).
	base := int64(1_700_000_000)
	base = base - base%60 // align to a bucket boundary
	s.AddReading(a.ID, base+0, 40)
	s.AddReading(a.ID, base+60, 41)
	s.AddReading(a.ID, base+120, 42)
	s.AddReading(a.ID, base+180, 43)
	s.AddReading(b.ID, base+0, 50)
	s.AddReading(b.ID, base+60, 51)
	// b has no reading in bucket 120 -> gap
	s.AddReading(b.ID, base+180, 53)

	h, err := s.History([]string{a.ID, b.ID}, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Data) != 3 {
		t.Fatalf("expected [ts, a, b], got %d rows", len(h.Data))
	}
	ts := h.Data[0].([]int64)
	if len(ts) != 4 {
		t.Fatalf("expected 4 buckets, got %v", ts)
	}
	seriesB := h.Data[2].([]*float64)
	// Bucket index 2 (base+120) must be nil for b (the outage gap).
	if seriesB[2] != nil {
		t.Fatalf("expected nil gap for b at bucket 120, got %v", *seriesB[2])
	}
	if seriesB[0] == nil || *seriesB[0] != 50 {
		t.Fatalf("expected b[0]=50, got %v", seriesB[0])
	}
	seriesA := h.Data[1].([]*float64)
	if seriesA[2] == nil || *seriesA[2] != 42 {
		t.Fatalf("expected a[2]=42, got %v", seriesA[2])
	}
}

func TestCountReadingsAbove(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.CreateMachine("a", 1000)
	base := int64(1_700_000_000)
	// In-window readings: 48 (below), 50 (at), 55, 62 -> 3 at/above 50, max 62.
	s.AddReading(m.ID, base+10, 48)
	s.AddReading(m.ID, base+20, 50)
	s.AddReading(m.ID, base+30, 55)
	s.AddReading(m.ID, base+40, 62)
	// Out-of-window hot reading must not count.
	s.AddReading(m.ID, base-1000, 90)

	count, maxT, err := s.CountReadingsAbove(m.ID, base, 50)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 readings >=50 in window, got %d", count)
	}
	if maxT == nil || *maxT != 62 {
		t.Fatalf("expected max 62, got %v", maxT)
	}

	// None above a high threshold -> zero count, nil max.
	count, maxT, err = s.CountReadingsAbove(m.ID, base, 99)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || maxT != nil {
		t.Fatalf("expected 0/nil, got %d/%v", count, maxT)
	}
}

func TestMigrationAddsAggColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	// Create a DB with the pre-agg_notified alert_state schema and a row.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE alert_state (
	  machine_id TEXT PRIMARY KEY,
	  alerting INTEGER NOT NULL DEFAULT 0,
	  last_notified INTEGER NOT NULL DEFAULT 0,
	  stale_notified INTEGER NOT NULL DEFAULT 0);
	INSERT INTO alert_state(machine_id, alerting) VALUES('m1', 1);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// OpenStore must migrate the old DB (add agg_notified) without error.
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open/migrate old db: %v", err)
	}
	st, err := s.GetAlertState("m1")
	if err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if !st.Alerting || st.AggNotified {
		t.Fatalf("expected alerting kept and agg_notified defaulting false, got %+v", st)
	}
	// Writing the new field works after migration.
	if err := s.SaveAlertState("m1", AlertState{AggNotified: true}); err != nil {
		t.Fatal(err)
	}
	st, _ = s.GetAlertState("m1")
	if !st.AggNotified {
		t.Fatal("agg_notified should persist after migration")
	}
}

func TestAlertStatePersistsAggNotified(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveAlertState("m1", AlertState{Alerting: true, AggNotified: true, LastNotified: 42}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAlertState("m1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.AggNotified || !got.Alerting || got.LastNotified != 42 {
		t.Fatalf("state did not round-trip: %+v", got)
	}
}

func TestHistoryBucketKeepsMaxSpike(t *testing.T) {
	s := newTestStore(t)
	m, _ := s.CreateMachine("a", 1000)
	base := int64(1_700_000_040)
	base = base - base%60 // align to bucket boundary
	// Two readings in the same 60s bucket: a hot spike then a cooler reading.
	s.AddReading(m.ID, base+0, 57) // spike (would trip an alert)
	s.AddReading(m.ID, base+30, 46) // cooler, arrives later in the same bucket

	h, err := s.History([]string{m.ID}, base)
	if err != nil {
		t.Fatal(err)
	}
	series := h.Data[1].([]*float64)
	if len(series) != 1 || series[0] == nil {
		t.Fatalf("expected one non-nil bucket, got %v", series)
	}
	if *series[0] != 57 {
		t.Fatalf("bucket must keep the max (57) spike, got %v", *series[0])
	}
}

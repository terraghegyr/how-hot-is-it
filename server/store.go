package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database. modernc.org/sqlite is a pure-Go driver so the
// binary builds with CGO_ENABLED=0.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS machines (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS readings (
  machine_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  temp_c REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_readings ON readings(machine_id, ts);

CREATE TABLE IF NOT EXISTS alert_state (
  machine_id TEXT PRIMARY KEY,
  alerting INTEGER NOT NULL DEFAULT 0,
  last_notified INTEGER NOT NULL DEFAULT 0,
  stale_notified INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS alerts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  machine_id TEXT NOT NULL,
  machine_name TEXT NOT NULL,
  ts INTEGER NOT NULL,
  type TEXT NOT NULL,
  temp_c REAL,
  telegram_ok INTEGER NOT NULL
);
`

const maxAlertRows = 500

// OpenStore opens (or creates) the database at path in WAL mode and bootstraps
// the schema. Use ":memory:" for tests.
func OpenStore(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single shared in-memory DB survives only while one conn is open; and WAL
	// with the pure-Go driver is happiest without a large connection pool.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Machine is a monitored host.
type Machine struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	CreatedAt int64    `json:"created_at"`
	LatestC   *float64 `json:"latest_c"`  // nil until first report
	LatestTS  *int64   `json:"latest_ts"` // nil until first report
}

func newID() (string, error) {
	b := make([]byte, 4) // 4 bytes -> 8 hex chars
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CreateMachine inserts a new machine with a random 8-char hex id.
func (s *Store) CreateMachine(name string, now int64) (Machine, error) {
	id, err := newID()
	if err != nil {
		return Machine{}, err
	}
	_, err = s.db.Exec(`INSERT INTO machines(id, name, created_at) VALUES(?,?,?)`, id, name, now)
	if err != nil {
		return Machine{}, err
	}
	return Machine{ID: id, Name: name, CreatedAt: now}, nil
}

// ListMachines returns all machines with their latest reading (nil if none).
func (s *Store) ListMachines() ([]Machine, error) {
	rows, err := s.db.Query(`
SELECT m.id, m.name, m.created_at,
  (SELECT temp_c FROM readings r WHERE r.machine_id=m.id ORDER BY ts DESC LIMIT 1),
  (SELECT ts     FROM readings r WHERE r.machine_id=m.id ORDER BY ts DESC LIMIT 1)
FROM machines m
ORDER BY m.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		var m Machine
		if err := rows.Scan(&m.ID, &m.Name, &m.CreatedAt, &m.LatestC, &m.LatestTS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MachineName returns the machine name and whether it exists.
func (s *Store) MachineName(id string) (string, bool, error) {
	var name string
	err := s.db.QueryRow(`SELECT name FROM machines WHERE id=?`, id).Scan(&name)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

// DeleteMachine removes a machine plus its readings and alert_state in one
// transaction. Alert history rows are intentionally kept. Returns false if the
// machine did not exist.
func (s *Store) DeleteMachine(id string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM machines WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if _, err := tx.Exec(`DELETE FROM readings WHERE machine_id=?`, id); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM alert_state WHERE machine_id=?`, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// AddReading records a server-timestamped reading.
func (s *Store) AddReading(machineID string, ts int64, temp float64) error {
	_, err := s.db.Exec(`INSERT INTO readings(machine_id, ts, temp_c) VALUES(?,?,?)`, machineID, ts, temp)
	return err
}

// PruneReadings deletes readings older than cutoff (unix seconds).
func (s *Store) PruneReadings(cutoff int64) error {
	_, err := s.db.Exec(`DELETE FROM readings WHERE ts < ?`, cutoff)
	return err
}

// GetAlertState returns the persisted alert state for a machine (zero value if
// none stored yet).
func (s *Store) GetAlertState(id string) (AlertState, error) {
	var st AlertState
	var alerting, stale int
	err := s.db.QueryRow(`SELECT alerting, last_notified, stale_notified FROM alert_state WHERE machine_id=?`, id).
		Scan(&alerting, &st.LastNotified, &stale)
	if err == sql.ErrNoRows {
		return AlertState{}, nil
	}
	if err != nil {
		return AlertState{}, err
	}
	st.Alerting = alerting != 0
	st.StaleNotified = stale != 0
	return st, nil
}

// SaveAlertState upserts the alert state for a machine.
func (s *Store) SaveAlertState(id string, st AlertState) error {
	_, err := s.db.Exec(`
INSERT INTO alert_state(machine_id, alerting, last_notified, stale_notified)
VALUES(?,?,?,?)
ON CONFLICT(machine_id) DO UPDATE SET
  alerting=excluded.alerting,
  last_notified=excluded.last_notified,
  stale_notified=excluded.stale_notified`,
		id, b2i(st.Alerting), st.LastNotified, b2i(st.StaleNotified))
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// AlertRow is one row of alert history.
type AlertRow struct {
	ID          int64    `json:"id"`
	MachineID   string   `json:"machine_id"`
	MachineName string   `json:"machine_name"`
	TS          int64    `json:"ts"`
	Type        string   `json:"type"`
	TempC       *float64 `json:"temp_c"`
	TelegramOK  bool     `json:"telegram_ok"`
}

// InsertAlert appends a row to the alert history.
func (s *Store) InsertAlert(a AlertRow) error {
	_, err := s.db.Exec(`
INSERT INTO alerts(machine_id, machine_name, ts, type, temp_c, telegram_ok)
VALUES(?,?,?,?,?,?)`,
		a.MachineID, a.MachineName, a.TS, a.Type, a.TempC, b2i(a.TelegramOK))
	return err
}

// ListAlerts returns the most recent alert rows, newest first, capped at limit.
func (s *Store) ListAlerts(limit int) ([]AlertRow, error) {
	rows, err := s.db.Query(`
SELECT id, machine_id, machine_name, ts, type, temp_c, telegram_ok
FROM alerts ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlertRow{}
	for rows.Next() {
		var a AlertRow
		var ok int
		if err := rows.Scan(&a.ID, &a.MachineID, &a.MachineName, &a.TS, &a.Type, &a.TempC, &ok); err != nil {
			return nil, err
		}
		a.TelegramOK = ok != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// CapAlerts keeps only the newest maxAlertRows rows.
func (s *Store) CapAlerts() error {
	_, err := s.db.Exec(`DELETE FROM alerts WHERE id NOT IN (SELECT id FROM alerts ORDER BY id DESC LIMIT ?)`, maxAlertRows)
	return err
}

// LatestReading returns the latest reading for a machine as an alert.Reading.
func (s *Store) LatestReading(id string) (Reading, error) {
	var r Reading
	err := s.db.QueryRow(`SELECT temp_c, ts FROM readings WHERE machine_id=? ORDER BY ts DESC LIMIT 1`, id).
		Scan(&r.TempC, &r.TS)
	if err == sql.ErrNoRows {
		return Reading{HasData: false}, nil
	}
	if err != nil {
		return Reading{}, err
	}
	r.HasData = true
	return r, nil
}

// History returns uPlot-native columnar data for the given machine ids over
// readings with ts >= since. Readings are bucketed to 60s so multiple machines
// share a clean, aligned time axis; buckets a machine has no reading in are null,
// so gaps (outages) break the line instead of interpolating.
type History struct {
	IDs  []string `json:"ids"`
	Data []any    `json:"data"` // [ []int64 timestamps, []*float64 per machine... ]
}

func (s *Store) History(ids []string, since int64) (History, error) {
	h := History{IDs: ids, Data: []any{}}

	perMachine := make([]map[int64]float64, len(ids))
	bucketSet := map[int64]struct{}{}
	for i, id := range ids {
		m := map[int64]float64{}
		rows, err := s.db.Query(`SELECT ts, temp_c FROM readings WHERE machine_id=? AND ts>=? ORDER BY ts`, id, since)
		if err != nil {
			return h, err
		}
		for rows.Next() {
			var ts int64
			var t float64
			if err := rows.Scan(&ts, &t); err != nil {
				rows.Close()
				return h, err
			}
			b := ts - ts%60
			m[b] = t // last reading in the bucket wins
			bucketSet[b] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return h, err
		}
		rows.Close()
		perMachine[i] = m
	}

	buckets := make([]int64, 0, len(bucketSet))
	for b := range bucketSet {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })

	h.Data = append(h.Data, buckets)
	for i := range ids {
		series := make([]*float64, len(buckets))
		for j, b := range buckets {
			if v, ok := perMachine[i][b]; ok {
				vv := v
				series[j] = &vv
			}
		}
		h.Data = append(h.Data, series)
	}
	return h, nil
}

// AllMachineIDs returns every machine id (used for ids=all history requests).
func (s *Store) AllMachineIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM machines ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// parseIDs splits a comma-separated ids param, trimming blanks.
func parseIDs(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

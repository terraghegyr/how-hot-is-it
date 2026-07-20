package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"
)

func newTestServer(t *testing.T, s *Store, now func() time.Time) *httptest.Server {
	t.Helper()
	return newTestServerHook(t, s, now, nil)
}

func newTestServerHook(t *testing.T, s *Store, now func() time.Time, onReport func(string, string)) *httptest.Server {
	t.Helper()
	var webFS fs.FS = fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}}
	srv := httptest.NewServer(NewServer(s, testThreshold, 0, webFS, now, onReport))
	t.Cleanup(srv.Close)
	return srv
}

func TestConfigEndpointExposesThresholds(t *testing.T) {
	s := newTestStore(t)
	var webFS fs.FS = fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("ok")}}
	srv := httptest.NewServer(NewServer(s, 80, 50, webFS, fixedClock(1), nil))
	t.Cleanup(srv.Close)

	_, body := doJSON(t, "GET", srv.URL+"/api/config", nil)
	var cfg struct {
		Alert float64 `json:"alert_threshold_c"`
		Agg   float64 `json:"aggregated_threshold_c"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Alert != 80 || cfg.Agg != 50 {
		t.Fatalf("expected 80/50, got %v/%v", cfg.Alert, cfg.Agg)
	}
}

func doJSON(t *testing.T, method, url string, body any) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if raw, ok := body.(string); ok {
			buf.WriteString(raw)
		} else {
			json.NewEncoder(&buf).Encode(body)
		}
	}
	req, _ := http.NewRequest(method, url, &buf)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	data := make([]byte, 0)
	buf2 := new(bytes.Buffer)
	buf2.ReadFrom(resp.Body)
	resp.Body.Close()
	data = buf2.Bytes()
	return resp, data
}

func fixedClock(sec int64) func() time.Time {
	return func() time.Time { return time.Unix(sec, 0) }
}

func TestEnrollmentRoundTrip(t *testing.T) {
	s := newTestStore(t)
	srv := newTestServer(t, s, fixedClock(1_700_000_000))

	// Create machine.
	resp, body := doJSON(t, "POST", srv.URL+"/api/machines", map[string]string{"name": "webserver"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}
	var m Machine
	json.Unmarshal(body, &m)
	if len(m.ID) != 8 {
		t.Fatalf("expected 8-char id, got %q", m.ID)
	}

	// Before any report, latest is nil (grey).
	_, body = doJSON(t, "GET", srv.URL+"/api/machines", nil)
	var list []Machine
	json.Unmarshal(body, &list)
	if len(list) != 1 || list[0].LatestC != nil {
		t.Fatalf("expected 1 machine with nil latest, got %+v", list)
	}

	// Report a temperature.
	resp, _ = doJSON(t, "POST", srv.URL+"/api/report", map[string]any{"machine_id": m.ID, "temp_c": 55.5})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("report: expected 204, got %d", resp.StatusCode)
	}

	// Now machine shows live temp.
	_, body = doJSON(t, "GET", srv.URL+"/api/machines", nil)
	json.Unmarshal(body, &list)
	if list[0].LatestC == nil || *list[0].LatestC != 55.5 {
		t.Fatalf("expected latest 55.5, got %+v", list[0].LatestC)
	}
}

func TestReportUnknownMachine404(t *testing.T) {
	s := newTestStore(t)
	srv := newTestServer(t, s, fixedClock(1_700_000_000))

	resp, _ := doJSON(t, "POST", srv.URL+"/api/report", map[string]any{"machine_id": "deadbeef", "temp_c": 60})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM readings`); got != 0 {
		t.Fatalf("unknown report should insert no rows, got %d", got)
	}
}

func TestReportMalformedJSON400(t *testing.T) {
	s := newTestStore(t)
	srv := newTestServer(t, s, fixedClock(1_700_000_000))
	resp, _ := doJSON(t, "POST", srv.URL+"/api/report", "{not json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestDeleteCascadeAPI(t *testing.T) {
	s := newTestStore(t)
	srv := newTestServer(t, s, fixedClock(1_700_000_000))
	_, body := doJSON(t, "POST", srv.URL+"/api/machines", map[string]string{"name": "gone"})
	var m Machine
	json.Unmarshal(body, &m)
	doJSON(t, "POST", srv.URL+"/api/report", map[string]any{"machine_id": m.ID, "temp_c": 60})

	resp, _ := doJSON(t, "DELETE", srv.URL+"/api/machines/"+m.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
	// Its agent now gets 404.
	resp, _ = doJSON(t, "POST", srv.URL+"/api/report", map[string]any{"machine_id": m.ID, "temp_c": 60})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
	// Deleting again -> 404.
	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/machines/"+m.ID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on second delete, got %d", resp.StatusCode)
	}
}

func TestHistoryAPIGaps(t *testing.T) {
	s := newTestStore(t)
	base := int64(1_700_000_000)
	base = base - base%60
	// Clock returns base+200 so the 24h window includes our buckets.
	srv := newTestServer(t, s, fixedClock(base+200))
	m, _ := s.CreateMachine("a", base)
	s.AddReading(m.ID, base, 40)
	// gap at base+60
	s.AddReading(m.ID, base+120, 42)

	_, body := doJSON(t, "GET", srv.URL+"/api/history?ids=all", nil)
	var h struct {
		IDs  []string     `json:"ids"`
		Data [][]*float64 `json:"data"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("decode history: %v (%s)", err, body)
	}
	if len(h.Data) != 2 {
		t.Fatalf("expected [ts, series], got %d", len(h.Data))
	}
	ts := h.Data[0]
	series := h.Data[1]
	// Two distinct buckets present (base, base+120); the gap bucket is not in the
	// union of timestamps, so the series has exactly 2 entries, both non-nil.
	if len(ts) != 2 || len(series) != 2 {
		t.Fatalf("expected 2 buckets, got ts=%d series=%d", len(ts), len(series))
	}
	for i, v := range series {
		if v == nil {
			t.Fatalf("series[%d] unexpectedly nil", i)
		}
	}
}

func TestAlertsOrderingAndLimit(t *testing.T) {
	s := newTestStore(t)
	srv := newTestServer(t, s, fixedClock(1_700_000_000))
	for i := 0; i < 5; i++ {
		s.InsertAlert(AlertRow{MachineID: "x", MachineName: "x", TS: int64(i), Type: "breach", TelegramOK: true})
	}

	// Newest first.
	_, body := doJSON(t, "GET", srv.URL+"/api/alerts?limit=50", nil)
	var alerts []AlertRow
	json.Unmarshal(body, &alerts)
	if len(alerts) != 5 || alerts[0].TS != 4 || alerts[4].TS != 0 {
		t.Fatalf("expected newest-first ordering, got %+v", alerts)
	}

	// limit clamps to >=1.
	_, body = doJSON(t, "GET", srv.URL+"/api/alerts?limit=0", nil)
	json.Unmarshal(body, &alerts)
	if len(alerts) != 1 {
		t.Fatalf("expected limit clamped to 1, got %d", len(alerts))
	}

	// limit above max clamps to maxAlertRows (no error).
	resp, _ := doJSON(t, "GET", srv.URL+"/api/alerts?limit=99999", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for large limit, got %d", resp.StatusCode)
	}
}

func TestCreateMachineRequiresName(t *testing.T) {
	s := newTestStore(t)
	srv := newTestServer(t, s, fixedClock(1_700_000_000))
	resp, _ := doJSON(t, "POST", srv.URL+"/api/machines", map[string]string{"name": "   "})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for blank name, got %d", resp.StatusCode)
	}
}

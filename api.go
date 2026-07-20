package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Server holds dependencies for the HTTP handlers.
type Server struct {
	store     *Store
	threshold float64
	webFS     fs.FS
	now       func() time.Time
}

// NewServer builds the http.Handler with all routes registered.
func NewServer(store *Store, threshold float64, webFS fs.FS, now func() time.Time) http.Handler {
	s := &Server{store: store, threshold: threshold, webFS: webFS, now: now}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/report", s.handleReport)
	mux.HandleFunc("/api/machines", s.handleMachines)
	mux.HandleFunc("/api/machines/", s.handleMachineByID)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/alerts", s.handleAlerts)
	mux.Handle("/", http.FileServer(http.FS(webFS)))
	// Expose the configured threshold so the UI can draw the reference line.
	mux.HandleFunc("/api/config", s.handleConfig)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"alert_threshold_c": s.threshold})
}

// POST /api/report {machine_id, temp_c}
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		MachineID string  `json:"machine_id"`
		TempC     float64 `json:"temp_c"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || body.MachineID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_, ok, err := s.store.MachineName(body.MachineID)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if !ok {
		// Unknown machine: discard, no auto-enroll.
		http.Error(w, "unknown machine", http.StatusNotFound)
		return
	}
	if err := s.store.AddReading(body.MachineID, s.now().Unix(), body.TempC); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/machines  |  POST /api/machines {name}
func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		machines, err := s.store.ListMachines()
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		if machines == nil {
			machines = []Machine{}
		}
		writeJSON(w, http.StatusOK, machines)
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		m, err := s.store.CreateMachine(name, s.now().Unix())
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, m)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// DELETE /api/machines/{id}
func (s *Server) handleMachineByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/machines/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ok, err := s.store.DeleteMachine(id)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/history?ids=a,b,c  (ids=all for every machine)
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := r.URL.Query().Get("ids")
	var ids []string
	var err error
	if raw == "all" || raw == "" {
		ids, err = s.store.AllMachineIDs()
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
	} else {
		ids = parseIDs(raw)
	}
	since := s.now().Add(-readingRetention).Unix()
	h, err := s.store.History(ids, since)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// GET /api/alerts?limit=50
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > maxAlertRows {
		limit = maxAlertRows
	}
	alerts, err := s.store.ListAlerts(limit)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, alerts)
}

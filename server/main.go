package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"time"
)

//go:embed web
var webRoot embed.FS

func main() {
	configPath := flag.String("config", "config.json", "path to config JSON file")
	dbPath := flag.String("db", "howhot.db", "path to SQLite database file")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	store, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	webFS, err := fs.Sub(webRoot, "web")
	if err != nil {
		log.Fatalf("web fs: %v", err)
	}

	now := time.Now
	mon := &Monitor{
		store:           store,
		notify:          NewTelegramNotifier(cfg),
		threshold:       cfg.AlertThresholdC,
		alertingEnabled: cfg.AlertingEnabled(),
		now:             now,
		aggEnabled:      cfg.AggregatedEnabled(),
		aggThreshold:    cfg.AggregatedThresholdC,
		aggCount:        cfg.AggregatedCount,
		aggWindow:       time.Duration(cfg.AggregatedWindowMinutes) * time.Minute,
	}
	stop := make(chan struct{})
	defer close(stop)
	go mon.Run(60*time.Second, stop)

	// Evaluate alerts immediately on ingest, in the background so a slow Telegram
	// send never blocks the agent's POST.
	onReport := func(machineID, name string) {
		go func() {
			if err := mon.EvaluateMachine(machineID, name); err != nil {
				log.Printf("ingest alert eval error: %v", err)
			}
		}()
	}
	// Only advertise the aggregated threshold to the UI when the feature is on.
	var aggThresholdUI float64
	if cfg.AggregatedEnabled() {
		aggThresholdUI = cfg.AggregatedThresholdC
	}
	handler := NewServer(store, cfg.AlertThresholdC, aggThresholdUI, webFS, now, onReport)

	addr := ":" + strconv.Itoa(cfg.ListenPort)
	log.Printf("how-hot-is-it listening on %s (threshold %.0f°C, alerting=%v, aggregated=%v)",
		addr, cfg.AlertThresholdC, cfg.AlertingEnabled(), cfg.AggregatedEnabled())
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("server: %v", err)
	}
}

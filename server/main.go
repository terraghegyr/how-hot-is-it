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
	}
	stop := make(chan struct{})
	defer close(stop)
	go mon.Run(60*time.Second, stop)

	handler := NewServer(store, cfg.AlertThresholdC, webFS, now)

	addr := ":" + strconv.Itoa(cfg.ListenPort)
	log.Printf("how-hot-is-it listening on %s (threshold %.0f°C, alerting=%v)", addr, cfg.AlertThresholdC, cfg.AlertingEnabled())
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("server: %v", err)
	}
}

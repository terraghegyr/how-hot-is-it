package main

import (
	"encoding/json"
	"os"
)

// Config is the entire server configuration, read once from a single JSON file.
// An empty TelegramBotToken disables alerting delivery; everything else still works.
type Config struct {
	TelegramBotToken string  `json:"telegram_bot_token"`
	TelegramChatID   string  `json:"telegram_chat_id"`
	AlertThresholdC  float64 `json:"alert_threshold_c"`
	ListenPort       int     `json:"listen_port"`
	// TelegramAPIBase lets tests point the sender at a fake endpoint. It defaults
	// to https://api.telegram.org and can be overridden here or via the
	// TELEGRAM_API_BASE environment variable.
	TelegramAPIBase string `json:"telegram_api_base"`

	// Aggregated alert: fire once when more than AggregatedCount readings are at
	// or above AggregatedThresholdC within the last AggregatedWindowMinutes. Meant
	// for sustained mild elevation below AlertThresholdC. Disabled unless all three
	// are > 0. No recovery message; suppressed while a main breach is active.
	AggregatedThresholdC    float64 `json:"aggregated_threshold_c"`
	AggregatedCount         int     `json:"aggregated_count"`
	AggregatedWindowMinutes int     `json:"aggregated_window_minutes"`
}

const defaultTelegramAPIBase = "https://api.telegram.org"

// LoadConfig reads config from path, applies defaults, and honours the
// TELEGRAM_API_BASE env override (env wins over the file so tests can inject it).
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	c.applyDefaults()
	return c, nil
}

func (c *Config) applyDefaults() {
	if c.ListenPort == 0 {
		c.ListenPort = 8080
	}
	if c.AlertThresholdC == 0 {
		c.AlertThresholdC = 80
	}
	if base := os.Getenv("TELEGRAM_API_BASE"); base != "" {
		c.TelegramAPIBase = base
	}
	if c.TelegramAPIBase == "" {
		c.TelegramAPIBase = defaultTelegramAPIBase
	}
}

// AlertingEnabled reports whether Telegram delivery is configured. When false,
// state transitions are still recorded in the alerts table (telegram_ok=1).
func (c Config) AlertingEnabled() bool {
	return c.TelegramBotToken != "" && c.TelegramChatID != ""
}

// AggregatedEnabled reports whether the aggregated alert is configured. All three
// values must be positive; any missing/zero field leaves it off.
func (c Config) AggregatedEnabled() bool {
	return c.AggregatedThresholdC > 0 && c.AggregatedCount > 0 && c.AggregatedWindowMinutes > 0
}

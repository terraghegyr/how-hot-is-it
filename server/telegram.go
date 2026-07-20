package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Notifier delivers alert messages. The real implementation posts to the Telegram
// Bot API; tests can supply their own or point the base URL at an httptest server.
type Notifier interface {
	Send(msg string) bool // returns true on successful delivery
}

// TelegramNotifier posts messages via the Telegram Bot API sendMessage method.
// BaseURL defaults to https://api.telegram.org and is overridable for testing.
type TelegramNotifier struct {
	BaseURL string
	Token   string
	ChatID  string
	Client  *http.Client
}

func NewTelegramNotifier(cfg Config) *TelegramNotifier {
	return &TelegramNotifier{
		BaseURL: cfg.TelegramAPIBase,
		Token:   cfg.TelegramBotToken,
		ChatID:  cfg.TelegramChatID,
		Client:  &http.Client{Timeout: 5 * time.Second},
	}
}

func (t *TelegramNotifier) Send(msg string) bool {
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(t.BaseURL, "/"), t.Token)
	form := url.Values{}
	form.Set("chat_id", t.ChatID)
	form.Set("text", msg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Drain a small amount so the connection can be reused.
	var discard json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&discard)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

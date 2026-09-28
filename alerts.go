package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

type Alerter struct {
	cfg  AlertConfig
	http *http.Client
	mu   sync.Mutex
	last map[string]time.Time
}

func NewAlerter(c AlertConfig) *Alerter {
	return &Alerter{cfg: c, http: &http.Client{Timeout: 10 * time.Second}, last: map[string]time.Time{}}
}

func (a *Alerter) Send(ctx context.Context, key, level, message string) error {
	a.mu.Lock()
	if last := a.last[key]; !last.IsZero() && time.Since(last) < time.Duration(a.cfg.CooldownSeconds)*time.Second {
		a.mu.Unlock()
		return nil
	}
	a.last[key] = time.Now()
	a.mu.Unlock()
	payload := map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "level": level, "message": message}
	var first error
	if endpoint := os.Getenv(a.cfg.WebhookURLEnv); endpoint != "" {
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := a.http.Do(req); err != nil {
			first = err
		} else {
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				first = fmt.Errorf("webhook HTTP %d", resp.StatusCode)
			}
		}
	}
	token, chat := os.Getenv(a.cfg.TelegramTokenEnv), os.Getenv(a.cfg.TelegramChatIDEnv)
	if token != "" && chat != "" {
		form := url.Values{"chat_id": {chat}, "text": {"[" + level + "] " + message}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/sendMessage", bytes.NewBufferString(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if resp, err := a.http.Do(req); err != nil && first == nil {
			first = err
		} else if err == nil {
			resp.Body.Close()
			if resp.StatusCode/100 != 2 && first == nil {
				first = fmt.Errorf("Telegram HTTP %d", resp.StatusCode)
			}
		}
	}
	return first
}

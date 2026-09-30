package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var dashboardHTML string

type dashboardAuth struct {
	AdminUsername  string
	AdminPassword  string
	ViewerUsername string
	ViewerPassword string
}

type dashboardActorKey struct{}

func startHTTP(ctxDone <-chan struct{}, listen string, e *Engine) (*http.Server, error) {
	if listen == "" {
		return nil, nil
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	auth := dashboardAuth{AdminUsername: e.cfg.Dashboard.Username, AdminPassword: os.Getenv(e.cfg.Dashboard.PasswordEnv), ViewerUsername: e.cfg.Dashboard.ReadOnlyUsername, ViewerPassword: os.Getenv(e.cfg.Dashboard.ReadOnlyPasswordEnv)}
	if e.cfg.Dashboard.AllowLAN || e.cfg.Mode == "live" {
		if auth.AdminPassword == "" {
			_ = listener.Close()
			return nil, errors.New("dashboard administrator password environment variable is missing")
		}
	}
	if auth.ViewerUsername != "" && auth.ViewerPassword == "" {
		_ = listener.Close()
		return nil, errors.New("dashboard read-only password environment variable is missing")
	}
	handler := dashboardHandlerWithAuth(e, auth)
	s := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctxDone
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdown)
	}()
	go func() {
		if err := s.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health server stopped", "error", err)
		}
	}()
	return s, nil
}

func dashboardHandler(e *Engine, password string) http.Handler {
	return dashboardHandlerWithAuth(e, dashboardAuth{AdminUsername: e.cfg.Dashboard.Username, AdminPassword: password})
}

func dashboardHandlerWithAuth(e *Engine, auth dashboardAuth) http.Handler {
	mux := http.NewServeMux()
	refreshSeconds := e.cfg.Dashboard.RefreshSeconds
	write := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		h := e.Health()
		if h.Status != "ok" && h.Status != "starting" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		write(w, h)
	})
	mux.HandleFunc("/v1/opportunities", func(w http.ResponseWriter, _ *http.Request) { write(w, e.Opportunities()) })
	mux.HandleFunc("/v1/funding", func(w http.ResponseWriter, _ *http.Request) { write(w, e.FundingWatch()) })
	mux.HandleFunc("/v1/positions", func(w http.ResponseWriter, _ *http.Request) { write(w, e.Health().Portfolio) })
	mux.HandleFunc("/v1/pending-operations", func(w http.ResponseWriter, _ *http.Request) { write(w, e.PendingOperations()) })
	mux.HandleFunc("/v1/ledger", func(w http.ResponseWriter, _ *http.Request) {
		rows, err := e.Ledger(200)
		if err != nil {
			http.Error(w, "ledger unavailable", http.StatusInternalServerError)
			return
		}
		write(w, rows)
	})
	mux.HandleFunc("/v1/settings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			write(w, e.settingsResponse())
		case http.MethodPost:
			if r.Header.Get("X-Config-Intent") != "save" {
				http.Error(w, "missing save intent", http.StatusForbidden)
				return
			}
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
			d := json.NewDecoder(r.Body)
			d.DisallowUnknownFields()
			var settings TuningSettings
			if err := d.Decode(&settings); err != nil {
				http.Error(w, "invalid settings JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := ensureJSONEOF(d); err != nil {
				http.Error(w, "invalid settings JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			result, err := e.updateSettings(settings)
			if err != nil {
				if errors.Is(err, errInvalidTuning) {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				slog.Error("save dashboard tuning", "error", err)
				http.Error(w, "settings could not be saved", http.StatusInternalServerError)
				return
			}
			actor, _ := r.Context().Value(dashboardActorKey{}).(string)
			_ = appendLedger(e.cfg.StateDir, map[string]any{"time": time.Now(), "event": "settings_audit", "actor": actor, "remote_addr": r.RemoteAddr})
			write(w, result)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		page := strings.ReplaceAll(dashboardHTML, "__REFRESH_SECONDS__", strconv.Itoa(refreshSeconds))
		_, _ = w.Write([]byte(page))
	})
	if auth.AdminPassword == "" {
		return mux
	}
	type failureWindow struct {
		Count        int
		BlockedUntil time.Time
	}
	var authMu sync.Mutex
	failures := map[string]failureWindow{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host == "" {
			host = r.RemoteAddr
		}
		authMu.Lock()
		window := failures[host]
		blocked := window.BlockedUntil.After(time.Now())
		authMu.Unlock()
		if blocked {
			http.Error(w, "too many authentication failures", http.StatusTooManyRequests)
			return
		}
		username, supplied, ok := r.BasicAuth()
		adminOK := ok && subtle.ConstantTimeCompare([]byte(username), []byte(auth.AdminUsername)) == 1 && subtle.ConstantTimeCompare([]byte(supplied), []byte(auth.AdminPassword)) == 1
		viewerOK := ok && auth.ViewerUsername != "" && subtle.ConstantTimeCompare([]byte(username), []byte(auth.ViewerUsername)) == 1 && subtle.ConstantTimeCompare([]byte(supplied), []byte(auth.ViewerPassword)) == 1
		if !adminOK && !viewerOK {
			authMu.Lock()
			window = failures[host]
			window.Count++
			if window.Count >= 5 {
				window.BlockedUntil = time.Now().Add(30 * time.Second)
				window.Count = 0
			}
			failures[host] = window
			authMu.Unlock()
			w.Header().Set("WWW-Authenticate", `Basic realm="Funding Monitor", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		authMu.Lock()
		delete(failures, host)
		authMu.Unlock()
		if viewerOK && r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only account", http.StatusForbidden)
			return
		}
		actor := "viewer:" + username
		if adminOK {
			actor = "admin:" + username
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), dashboardActorKey{}, actor)))
	})
}

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoadConfig(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.holidaysFile != "/config/holidays.txt" || cfg.addressesFile != "/data/addresses.json" ||
		cfg.listenAddr != ":8080" || cfg.refresh != 6*time.Hour {
		t.Errorf("defaults = %+v", cfg)
	}

	cfg, err = loadConfig(env(map[string]string{
		"HOLIDAYS_FILE":    "/data/h.txt",
		"ADDRESSES_FILE":   "/state/saved.json",
		"LISTEN_ADDR":      ":9000",
		"REFRESH_INTERVAL": "1h",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.holidaysFile != "/data/h.txt" || cfg.addressesFile != "/state/saved.json" || cfg.listenAddr != ":9000" ||
		cfg.refresh != time.Hour {
		t.Errorf("overrides = %+v", cfg)
	}
}

func TestPublicURL(t *testing.T) {
	tests := map[string]string{
		"":                                     "",
		"https://zackwag.com/public/trashday/": "https://zackwag.com/public/trashday",
		"http://192.168.4.70:3138":             "http://192.168.4.70:3138",
	}
	for in, want := range tests {
		cfg, err := loadConfig(env(map[string]string{"PUBLIC_URL": in}))
		if err != nil || cfg.publicURL != want {
			t.Errorf("PUBLIC_URL=%q: got %q, %v, want %q", in, cfg.publicURL, err, want)
		}
	}
	for _, bad := range []string{"zackwag.com/trashday", "ftp://zackwag.com", "https://", "https://x.com/?a=b", "https://x.com/#top", "http://[::1"} {
		if _, err := loadConfig(env(map[string]string{"PUBLIC_URL": bad})); err == nil || !strings.Contains(err.Error(), "PUBLIC_URL") {
			t.Errorf("PUBLIC_URL=%q: err = %v, want one about PUBLIC_URL", bad, err)
		}
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := map[string]map[string]string{
		"REFRESH_INTERVAL": {"REFRESH_INTERVAL": "daily"},
		"at least 1m":      {"REFRESH_INTERVAL": "10s"},
	}
	for want, vars := range tests {
		if _, err := loadConfig(env(vars)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("loadConfig(%v) = %v, want an error mentioning %q", vars, err, want)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	if code := healthcheck(":" + port); code != 0 {
		t.Errorf("healthcheck() = %d against a healthy server, want 0", code)
	}
	status.Store(http.StatusServiceUnavailable)
	if code := healthcheck("0.0.0.0:" + port); code != 1 {
		t.Errorf("healthcheck() = %d against an unavailable server, want 1", code)
	}
	if code := healthcheck("no-port"); code != 1 {
		t.Errorf("healthcheck() = %d with a bad address, want 1", code)
	}
	srv.Close()
	if code := healthcheck(":" + port); code != 1 {
		t.Errorf("healthcheck() = %d with nothing listening, want 1", code)
	}
}

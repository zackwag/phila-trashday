// Command phila-trashday serves Philadelphia trash and recycling schedules
// on the LAN: a web page at /, JSON at /schedule.json and an iCalendar feed
// at /schedule.ics, for any address, for each saved address under
// /a/{id}/, and for the default saved address when no address is given.
//
// It's configured with environment variables:
//
//	HOLIDAYS_FILE     optional extra holidays, one YYYY-MM-DD per line, on top of the city's computed ones (default /config/holidays.txt)
//	ADDRESSES_FILE    where saved addresses are kept (default /data/addresses.json)
//	PUBLIC_URL        where a reverse proxy serves saved addresses' feeds publicly, like https://example.com/trashday (optional)
//	ALARM_TIME        when feeds alert the evening before a pickup, as HH:MM, or "off" (default 20:00)
//	REFRESH_INTERVAL  how long an address lookup is reused, and how often the extra holidays file is re-read (default 6h)
//	LISTEN_ADDR       where to serve (default :8080)
//
// "phila-trashday healthcheck" checks a running instance's /healthz, for
// Docker's HEALTHCHECK in an image with no shell or curl.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the image is FROM scratch, so it has no zoneinfo

	"github.com/zackwag/phila-trashday/internal/ais"
	"github.com/zackwag/phila-trashday/internal/server"
	"github.com/zackwag/phila-trashday/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	holidaysFile  string
	addressesFile string
	publicURL     string
	alarm         *time.Duration
	refresh       time.Duration
	listenAddr    string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Getenv("LISTEN_ADDR")))
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return fmt.Errorf("load Philadelphia's time zone: %w", err)
	}

	svc := server.New(server.Config{
		Fetcher: &ais.Client{
			HTTP:      &http.Client{Timeout: 30 * time.Second},
			BaseURL:   ais.DefaultBaseURL,
			UserAgent: "phila-trashday/" + version + " (+https://github.com/zackwag/phila-trashday)",
		},
		HolidaysFile:  cfg.holidaysFile,
		AddressesFile: cfg.addressesFile,
		PublicURL:     cfg.publicURL,
		Alarm:         cfg.alarm,
		Refresh:       cfg.refresh,
		Location:      loc,
		Logger:        logger,
		Site:          web.Handler(),
	})
	if err := svc.LoadSaved(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		logger.Info("serving", "addr", cfg.listenAddr, "version", version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}
	return nil
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		holidaysFile:  or(getenv("HOLIDAYS_FILE"), "/config/holidays.txt"),
		addressesFile: or(getenv("ADDRESSES_FILE"), "/data/addresses.json"),
		listenAddr:    or(getenv("LISTEN_ADDR"), ":8080"),
	}

	if raw := getenv("PUBLIC_URL"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return config{}, fmt.Errorf("PUBLIC_URL must be an http or https URL with no query, like https://example.com/trashday, not %q", raw)
		}
		cfg.publicURL = strings.TrimSuffix(u.String(), "/")
	}

	refresh, err := time.ParseDuration(or(getenv("REFRESH_INTERVAL"), "6h"))
	if err != nil || refresh < time.Minute {
		return config{}, fmt.Errorf("REFRESH_INTERVAL must be a duration of at least 1m, like 6h, not %q", getenv("REFRESH_INTERVAL"))
	}
	cfg.refresh = refresh

	cfg.alarm, err = server.ParseAlarm(or(getenv("ALARM_TIME"), "20:00"))
	if err != nil {
		return config{}, fmt.Errorf("ALARM_TIME: %w", err)
	}
	return cfg, nil
}

// healthcheck asks the instance listening on listenAddr for /healthz and
// returns the exit code: 0 if it answers 200, 1 if not.
func healthcheck(listenAddr string) int {
	_, port, err := net.SplitHostPort(or(listenAddr, ":8080"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck:", resp.Status)
		return 1
	}
	return 0
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

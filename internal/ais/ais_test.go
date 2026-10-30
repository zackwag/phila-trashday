package ais

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zackwag/phila-trashday/internal/schedule"
)

// seen is what the test server recorded about the last request.
type seen struct {
	mu        sync.Mutex
	path      string
	userAgent string
}

func (s *seen) get() (path, userAgent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path, s.userAgent
}

// server answers every request with status and body, and records the last
// request's path and User-Agent.
func server(t *testing.T, status int, body string) (*Client, *seen) {
	t.Helper()
	last := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.mu.Lock()
		last.path, last.userAgent = r.URL.Path, r.Header.Get("User-Agent")
		last.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), BaseURL: srv.URL + "/", UserAgent: "test-agent"}, last
}

func feature(primary, secondary string) string {
	return featureAt(`"1830 FITZWATER ST"`, primary, secondary)
}

func featureAt(address, primary, secondary string) string {
	return `{"features":[{"properties":{"street_address":` + address + `,` +
		`"rubbish_recycle_day":` + primary + `,"secondary_rubbish_day":` + secondary + `}}]}`
}

func TestLookup(t *testing.T) {
	tests := []struct {
		name string
		body string
		want schedule.Days
	}{
		{
			name: "primary and secondary",
			body: feature(`"WED"`, `"SAT"`),
			want: schedule.Days{Primary: time.Wednesday, Secondary: time.Saturday, HasSecondary: true},
		},
		{
			name: "no secondary",
			body: feature(`"MON"`, `null`),
			want: schedule.Days{Primary: time.Monday},
		},
		{
			name: "secondary on Sunday",
			body: feature(`"fri"`, `"sun"`),
			want: schedule.Days{Primary: time.Friday, Secondary: time.Sunday, HasSecondary: true},
		},
		{
			name: "unknown secondary is ignored",
			body: feature(`"THU"`, `""`),
			want: schedule.Days{Primary: time.Thursday},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, req := server(t, http.StatusOK, tt.body)
			got, err := c.Lookup(context.Background(), " 1830 Fitzwater St ")
			if err != nil {
				t.Fatal(err)
			}
			if got.Days != tt.want {
				t.Errorf("Lookup().Days = %+v, want %+v", got.Days, tt.want)
			}
			if got.Address != "1830 FITZWATER ST" {
				t.Errorf("Lookup().Address = %q, want 1830 FITZWATER ST", got.Address)
			}
			path, userAgent := req.get()
			if want := "/ais/v1/addresses/1830 FITZWATER ST"; path != want {
				t.Errorf("path = %q, want %q", path, want)
			}
			if userAgent != "test-agent" {
				t.Errorf("User-Agent = %q, want test-agent", userAgent)
			}
		})
	}
}

func TestLookupWithoutStreetAddress(t *testing.T) {
	c, _ := server(t, http.StatusOK, featureAt(`null`, `"WED"`, `null`))
	got, err := c.Lookup(context.Background(), " 1830 fitzwater st ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != "1830 FITZWATER ST" {
		t.Errorf("Address = %q, want the query in AIS's case", got.Address)
	}
}

func TestLookupErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantErr  error
		contains string
	}{
		{name: "404", status: http.StatusNotFound, wantErr: ErrNotFound},
		{name: "no features", status: http.StatusOK, body: `{"features":[]}`, wantErr: ErrNotFound},
		{name: "server error", status: http.StatusBadGateway, contains: "502"},
		{name: "bad JSON", status: http.StatusOK, body: `<html>`, contains: "decode"},
		{name: "no primary day", status: http.StatusOK, body: feature(`null`, `null`), contains: "no collection day"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := server(t, tt.status, tt.body)
			_, err := c.Lookup(context.Background(), "1830 FITZWATER ST")
			if err == nil {
				t.Fatal("Lookup() succeeded, want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.contains != "" && !strings.Contains(err.Error(), tt.contains) {
				t.Errorf("err = %v, want it to mention %q", err, tt.contains)
			}
		})
	}
}

func TestLookupRequestFails(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:0"}
	if _, err := c.Lookup(context.Background(), "1830 FITZWATER ST"); err == nil {
		t.Error("Lookup() succeeded against a closed port")
	}

	c = &Client{BaseURL: "http://bad host"}
	if _, err := c.Lookup(context.Background(), "x"); err == nil {
		t.Error("Lookup() succeeded with an invalid URL")
	}
}

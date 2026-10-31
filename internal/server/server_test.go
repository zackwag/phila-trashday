package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/zackwag/phila-trashday/internal/ais"
	"github.com/zackwag/phila-trashday/internal/schedule"
)

var philly = mustLoad("America/New_York")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

var wedSat = schedule.Days{Primary: time.Wednesday, Secondary: time.Saturday, HasSecondary: true}

// fakeFetcher knows a set of addresses, can be made to fail, and counts
// lookups per address.
type fakeFetcher struct {
	mu    sync.Mutex
	known map[string]ais.Property
	err   error
	calls map[string]int
	// hook, if set, runs at the start of every lookup, without mu held.
	hook func(address string)
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		known: map[string]ais.Property{
			"1830 FITZWATER ST":        {Address: "1830 FITZWATER ST", Days: wedSat},
			"1830 FITZWATER STREET":    {Address: "1830 FITZWATER ST", Days: wedSat},
			"582 PAOLI AVE":            {Address: "582 PAOLI AVE", Days: schedule.Days{Primary: time.Monday}},
			"2714 S MARVINE ST":        {Address: "2714 S MARVINE ST", Days: schedule.Days{Primary: time.Friday}},
			"9868 COWDEN ST":           {Address: "9868 COWDEN ST", Days: schedule.Days{Primary: time.Tuesday}},
			"1600 JOHN F KENNEDY BLVD": {Address: "1600 JOHN F KENNEDY BLVD", Days: schedule.Days{Primary: time.Thursday}},
		},
		calls: map[string]int{},
	}
}

func (f *fakeFetcher) Lookup(ctx context.Context, address string) (ais.Property, error) {
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook(address)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[address]++
	if err := ctx.Err(); err != nil {
		return ais.Property{}, err
	}
	if f.err != nil {
		return ais.Property{}, f.err
	}
	p, ok := f.known[address]
	if !ok {
		return ais.Property{}, fmt.Errorf("%q: %w", address, ais.ErrNotFound)
	}
	return p, nil
}

func (f *fakeFetcher) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeFetcher) count(address string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[address]
}

// fixture is a Service over a fake fetcher, files in a temp directory and a
// clock the test controls.
type fixture struct {
	t        *testing.T
	svc      *Service
	fetcher  *fakeFetcher
	holidays string
	saved    string
	now      time.Time
}

func newFixture(t *testing.T, opts ...func(*Config)) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		t:        t,
		fetcher:  newFakeFetcher(),
		holidays: filepath.Join(dir, "holidays.txt"),
		saved:    filepath.Join(dir, "addresses.json"),
		now:      time.Date(2026, 10, 30, 12, 0, 0, 0, philly),
	}
	cfg := Config{
		Fetcher:       f.fetcher,
		HolidaysFile:  f.holidays,
		AddressesFile: f.saved,
		Refresh:       6 * time.Hour,
		Location:      philly,
		Now:           func() time.Time { return f.now },
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	f.svc = New(cfg)
	// The city's holidays are computed. The file adds one more.
	f.writeHolidays("2026-12-24 Christmas Eve\n")
	if err := f.svc.ReloadHolidays(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) writeHolidays(content string) {
	f.t.Helper()
	if err := os.WriteFile(f.holidays, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) do(method, path, contentType, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	f.svc.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *fixture) get(path string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(http.MethodGet, path, "", "")
}

// save posts an address to /api/addresses, with a label if it isn't empty.
func (f *fixture) save(address, label string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := savedRequest{Address: &address}
	if label != "" {
		req.Label = &label
	}
	body, err := json.Marshal(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.do(http.MethodPost, "/api/addresses", "application/json", string(body))
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func (f *fixture) schedule(address string) scheduleJSON {
	f.t.Helper()
	rec := f.get("/schedule.json?address=" + strings.ReplaceAll(address, " ", "+"))
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET /schedule.json for %q = %d: %s", address, rec.Code, rec.Body)
	}
	return decode[scheduleJSON](f.t, rec)
}

func hasWarning(warnings []string, substr string) bool {
	return slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, substr) })
}

func findCollection(t *testing.T, cs []collectionJSON, id string) collectionJSON {
	t.Helper()
	i := slices.IndexFunc(cs, func(c collectionJSON) bool { return c.ID == id })
	if i < 0 {
		t.Fatalf("no collection %s", id)
	}
	return cs[i]
}

func TestScheduleJSON(t *testing.T) {
	f := newFixture(t)
	got := f.schedule("1830 fitzwater  street")

	if got.Address != "1830 FITZWATER ST" || got.PrimaryDay != "Wednesday" || got.SecondaryDay != "Saturday" {
		t.Errorf("address and days = %q, %q, %q", got.Address, got.PrimaryDay, got.SecondaryDay)
	}
	// The city's holidays from October 23, 2026, with the extra one from
	// the file among them, and weekend ones left out.
	wantFirst := []holidayJSON{
		{"2026-11-11", "Veterans Day"},
		{"2026-11-26", "Thanksgiving"},
		{"2026-11-27", "Thanksgiving Friday"},
		{"2026-12-24", "Christmas Eve"},
		{"2026-12-25", "Christmas Day"},
		{"2027-01-01", "New Year's Day"},
	}
	if len(got.Holidays) < len(wantFirst) || !slices.Equal(got.Holidays[:len(wantFirst)], wantFirst) {
		t.Errorf("holidays = %v, want them to start %v", got.Holidays, wantFirst)
	}
	if slices.ContainsFunc(got.Holidays, func(h holidayJSON) bool { return h.Date == "2027-06-19" }) {
		t.Error("holidays include Juneteenth 2027, a Saturday")
	}
	if len(got.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", got.Warnings)
	}

	// A week back from October 30 is October 23, so the first pickup is
	// Saturday the 24th, and a year ahead ends October 30, 2027.
	first, last := got.Collections[0], got.Collections[len(got.Collections)-1]
	if first.Date != "2026-10-24" {
		t.Errorf("first collection = %s, want 2026-10-24", first.Date)
	}
	if last.Date > "2027-10-30" {
		t.Errorf("last collection = %s, want on or before 2027-10-30", last.Date)
	}

	c := findCollection(t, got.Collections, "2026-11-11-primary")
	if c.Date != "2026-11-12" || c.ScheduledDate != "2026-11-11" || !c.Moved || c.Slot != schedule.Primary ||
		!slices.Equal(c.Kinds, []schedule.Kind{schedule.Trash, schedule.Recycling}) || c.Summary != "Trash and recycling" {
		t.Errorf("Veterans Day pickup = %+v", c)
	}
}

func TestScheduleJSONWithoutSecondary(t *testing.T) {
	f := newFixture(t)
	if got := f.schedule("582 PAOLI AVE"); got.SecondaryDay != "" {
		t.Errorf("secondary_day = %q, want it left out", got.SecondaryDay)
	}
}

func TestBadRequests(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		path   string
		status int
		want   string
	}{
		{"/schedule.json", http.StatusBadRequest, "no address given"},
		{"/schedule.json?address=%20%20", http.StatusBadRequest, "no address given"},
		{"/schedule.json?address=" + strings.Repeat("A", maxAddressLen+1), http.StatusBadRequest, "at most"},
		{"/schedule.json?address=1830%20FITZWATER%20ST%3B%20DROP", http.StatusBadRequest, "can't be part of an address"},
		{"/schedule.json?address=1%20NOWHERE%20ST", http.StatusNotFound, "no property at 1 NOWHERE ST"},
		{"/schedule.ics?address=1%20NOWHERE%20ST", http.StatusNotFound, "no property"},
		{"/schedule.ics?address=1830%20FITZWATER%20ST&alarm=8pm", http.StatusBadRequest, "alarm must be HH:MM"},
	}
	for _, tt := range tests {
		rec := f.get(tt.path)
		if rec.Code != tt.status || !strings.Contains(decode[errorJSON](t, rec).Error, tt.want) {
			t.Errorf("GET %s = %d %s, want %d mentioning %q", tt.path, rec.Code, rec.Body, tt.status, tt.want)
		}
	}
}

func TestLookupFailure(t *testing.T) {
	f := newFixture(t)
	f.fetcher.fail(errors.New("connection refused"))
	rec := f.get("/schedule.json?address=1830+FITZWATER+ST")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("GET = %d %s, want 502 with the cause", rec.Code, rec.Body)
	}
}

func TestLookupsAreRemembered(t *testing.T) {
	f := newFixture(t)
	const address = "1830 FITZWATER ST"

	f.schedule(address)
	f.schedule(address)
	if n := f.fetcher.count(address); n != 1 {
		t.Errorf("looked up %d times within the refresh interval, want 1", n)
	}

	f.now = f.now.Add(6 * time.Hour)
	f.schedule(address)
	if n := f.fetcher.count(address); n != 2 {
		t.Errorf("looked up %d times after the refresh interval, want 2", n)
	}

	// Once the city starts failing, the last good days are served with a
	// warning, and the city isn't asked again until retryAfter has passed.
	f.fetcher.fail(errors.New("timeout"))
	f.now = f.now.Add(6 * time.Hour)
	failedAt := f.now
	got := f.schedule(address)
	if len(got.Collections) == 0 {
		t.Fatal("no collections after a failed lookup, want the last good days")
	}
	wantSince := "failed since " + failedAt.Format(time.RFC3339) + " (timeout)"
	if !hasWarning(got.Warnings, wantSince) {
		t.Errorf("warnings = %v, want %q", got.Warnings, wantSince)
	}
	f.now = f.now.Add(time.Minute)
	f.schedule(address)
	if n := f.fetcher.count(address); n != 3 {
		t.Errorf("looked up %d times within retryAfter of a failure, want 3", n)
	}
	f.now = f.now.Add(retryAfter)
	got = f.schedule(address)
	if n := f.fetcher.count(address); n != 4 {
		t.Errorf("looked up %d times after retryAfter, want 4", n)
	}
	if !hasWarning(got.Warnings, wantSince) {
		t.Errorf("warnings = %v, want the first failure's time kept", got.Warnings)
	}

	f.fetcher.fail(nil)
	f.now = f.now.Add(retryAfter)
	if got := f.schedule(address); len(got.Warnings) != 0 {
		t.Errorf("warnings after recovering = %v, want none", got.Warnings)
	}
}

func TestNotFoundIsRemembered(t *testing.T) {
	f := newFixture(t)
	for range 3 {
		f.get("/schedule.json?address=1+NOWHERE+ST")
	}
	if n := f.fetcher.count("1 NOWHERE ST"); n != 1 {
		t.Errorf("looked up a missing address %d times within retryAfter, want 1", n)
	}
}

func TestCancelledRequestDoesNotFailLookup(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/schedule.json?address=1830+FITZWATER+ST", nil)
	rec := httptest.NewRecorder()
	f.svc.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET with a cancelled request = %d, want the lookup to finish anyway", rec.Code)
	}
}

func TestLeastRecentlyUsedAddressIsForgotten(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MaxAddresses = 2 })

	f.schedule("1830 FITZWATER ST")
	f.now = f.now.Add(time.Minute)
	f.schedule("582 PAOLI AVE")
	f.now = f.now.Add(time.Minute)
	f.schedule("1830 FITZWATER ST") // now more recent than Paoli
	f.now = f.now.Add(time.Minute)
	f.schedule("2714 S MARVINE ST") // pushes out Paoli

	f.schedule("1830 FITZWATER ST")
	f.schedule("582 PAOLI AVE")
	if n := f.fetcher.count("1830 FITZWATER ST"); n != 1 {
		t.Errorf("Fitzwater looked up %d times, want 1 since it was used recently", n)
	}
	if n := f.fetcher.count("582 PAOLI AVE"); n != 2 {
		t.Errorf("Paoli looked up %d times, want 2 since it was forgotten", n)
	}
}

func TestScheduleICS(t *testing.T) {
	f := newFixture(t)

	rec := f.get("/schedule.ics?address=1830+Fitzwater+St")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /schedule.ics = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/calendar; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"X-WR-CALNAME:Trash and recycling",
		"REFRESH-INTERVAL;VALUE=DURATION:PT6H",
		"UID:2026-11-11-primary.1830-fitzwater-st@phila-trashday",
		"DTSTART;VALUE=DATE:20261112",
		`DESCRIPTION:Moved from Wednesday\, November 11 for a holiday.`,
		"TRIGGER:-PT4H",
		"UID:2026-11-07-secondary.1830-fitzwater-st@phila-trashday",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("feed is missing %q", want)
		}
	}
	for _, cancelled := range []string{"20261114", "20261128"} {
		if strings.Contains(body, "DTSTART;VALUE=DATE:"+cancelled) {
			t.Errorf("feed has a pickup on %s, want it cancelled", cancelled)
		}
	}
}

func TestICSAlarmParameter(t *testing.T) {
	f := newFixture(t)
	if body := f.get("/schedule.ics?address=1830+FITZWATER+ST&alarm=21:30").Body.String(); !strings.Contains(body, "TRIGGER:-PT2H30M") {
		t.Error("alarm=21:30 didn't set a 9:30 PM alert")
	}
	if body := f.get("/schedule.ics?address=1830+FITZWATER+ST&alarm=off").Body.String(); strings.Contains(body, "VALARM") {
		t.Error("alarm=off still has alerts")
	}
}

func TestHealth(t *testing.T) {
	f := newFixture(t)
	if rec := f.get("/healthz"); rec.Code != http.StatusOK || decode[healthJSON](t, rec).Status != "ok" {
		t.Errorf("health = %d %s, want ok", rec.Code, rec.Body)
	}

	// Holidays are computed, so a new year needs nothing.
	f.now = time.Date(2027, 12, 20, 12, 0, 0, 0, philly)
	if h := decode[healthJSON](t, f.get("/healthz")); h.Status != "ok" {
		t.Errorf("health in December 2027 = %+v, want ok", h)
	}

	f.writeHolidays("bad\n")
	_ = f.svc.ReloadHolidays()
	rec := f.get("/healthz")
	h := decode[healthJSON](t, rec)
	if rec.Code != http.StatusOK || h.Status != "degraded" || !hasWarning(h.Warnings, "extra holidays file") {
		t.Errorf("health with a bad file = %d %+v, want degraded about the file", rec.Code, h)
	}
	if got := f.schedule("1830 FITZWATER ST"); !hasWarning(got.Warnings, "extra holidays file") {
		t.Errorf("schedule warnings = %v, want the file warning too", got.Warnings)
	}
}

func TestHolidaysFileProblems(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		f := newFixture(t)
		if err := os.Remove(f.holidays); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.ReloadHolidays(); err != nil {
			t.Fatal(err)
		}
		got := f.schedule("1830 FITZWATER ST")
		if len(got.Warnings) != 0 {
			t.Errorf("warnings = %v, want none: the file is optional", got.Warnings)
		}
		if !findCollection(t, got.Collections, "2026-11-11-primary").Moved {
			t.Error("Veterans Day pickup not moved without a file, want the city's holidays still applied")
		}
		if slices.ContainsFunc(got.Holidays, func(h holidayJSON) bool { return h.Name == "Christmas Eve" }) {
			t.Error("the removed file's extra holiday is still listed")
		}
	})

	t.Run("bad line keeps the last good copy", func(t *testing.T) {
		f := newFixture(t)
		f.writeHolidays("November 11\n")
		if err := f.svc.ReloadHolidays(); err == nil || !strings.Contains(err.Error(), "line 1") {
			t.Fatalf("ReloadHolidays() = %v, want a parse error", err)
		}
		got := f.schedule("1830 FITZWATER ST")
		if !hasWarning(got.Warnings, "last good copy") {
			t.Errorf("warnings = %v, want one about the bad file", got.Warnings)
		}
		if !slices.ContainsFunc(got.Holidays, func(h holidayJSON) bool { return h.Name == "Christmas Eve" }) {
			t.Error("Christmas Eve gone, want the last good file kept")
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		f := newFixture(t)
		f.svc.cfg.HolidaysFile = t.TempDir() // a directory opens but can't be read
		if err := f.svc.ReloadHolidays(); err == nil {
			t.Fatal("ReloadHolidays() succeeded reading a directory")
		}
	})

	t.Run("can't open", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can open anything")
		}
		f := newFixture(t)
		if err := os.Chmod(f.holidays, 0); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.ReloadHolidays(); err == nil || !strings.Contains(err.Error(), "open holidays file") {
			t.Fatalf("ReloadHolidays() = %v, want an open error", err)
		}
	})
}

func TestRun(t *testing.T) {
	f := newFixture(t)
	f.writeHolidays("2027-03-01 Snow day\n")
	f.svc.cfg.Refresh = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.svc.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		f.svc.mu.RLock()
		_, reloaded := f.svc.extra[time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)]
		f.svc.mu.RUnlock()
		if reloaded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never reloaded the holidays")
		}
		time.Sleep(time.Millisecond)
	}

	// A bad file is logged and the loop keeps going.
	f.writeHolidays("bad\n")
	time.Sleep(5 * time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
}

func TestSite(t *testing.T) {
	site := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("the page at " + r.URL.Path))
	})
	f := newFixture(t, func(c *Config) { c.Site = site })
	if body := f.get("/").Body.String(); body != "the page at /" {
		t.Errorf("GET / = %q, want the site", body)
	}
	// A saved address's page is the site's page.
	if body := f.get("/a/" + uuid.NewV4().String()).Body.String(); body != "the page at /" {
		t.Errorf("GET /a/{id} = %q, want the site's page", body)
	}
	if rec := f.get("/schedule.json?address=1830+FITZWATER+ST"); rec.Code != http.StatusOK {
		t.Errorf("API behind the site = %d", rec.Code)
	}

	if rec := newFixture(t).get("/"); rec.Code != http.StatusNotFound {
		t.Errorf("GET / without a site = %d, want 404", rec.Code)
	}
}

func TestNewDefaults(t *testing.T) {
	svc := New(Config{Fetcher: newFakeFetcher()})
	rec := httptest.NewRecorder()
	svc.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/schedule.json?address=582+PAOLI+AVE", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /schedule.json = %d, want 200", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	if rec := f.do(http.MethodPost, "/schedule.json", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /schedule.json = %d, want 405", rec.Code)
	}
}

// brokenWriter is a ResponseWriter whose body writes fail.
type brokenWriter struct {
	header http.Header
}

func (w *brokenWriter) Header() http.Header       { return w.header }
func (w *brokenWriter) WriteHeader(int)           {}
func (w *brokenWriter) Write([]byte) (int, error) { return 0, errors.New("client went away") }

func TestWriteErrorsAreLogged(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/schedule.json?address=1830+FITZWATER+ST", "/schedule.ics?address=1830+FITZWATER+ST"} {
		w := &brokenWriter{header: http.Header{}}
		f.svc.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	}
}

func TestParseAlarm(t *testing.T) {
	tests := map[string]time.Duration{
		"20:00": -4 * time.Hour,
		"21:30": -(2*time.Hour + 30*time.Minute),
		"00:00": -24 * time.Hour,
		"8:05":  -(15*time.Hour + 55*time.Minute),
	}
	for in, want := range tests {
		got, err := parseAlarm(in)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || *got != want {
			t.Errorf("parseAlarm(%q) = %v, want %v", in, got, want)
		}
	}
	if got, err := parseAlarm("OFF"); err != nil || got != nil {
		t.Errorf("parseAlarm(OFF) = %v, %v, want no alarm", got, err)
	}
	if _, err := parseAlarm("8pm"); err == nil {
		t.Error("parseAlarm(8pm) succeeded")
	}
}

func TestNormalizeAlarm(t *testing.T) {
	tests := map[string]string{"20:00": "20:00", "8:05": "08:05", "Off": "off"}
	for in, want := range tests {
		if got, err := normalizeAlarm(in); err != nil || got != want {
			t.Errorf("normalizeAlarm(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
}

func TestCleanAddress(t *testing.T) {
	tests := map[string]string{
		"  1830   fitzwater st ":  "1830 FITZWATER ST",
		"100 S 2ND ST #3":         "100 S 2ND ST #3",
		"1/2 O'NEIL ST, UNIT A-1": "1/2 O'NEIL ST, UNIT A-1",
	}
	for in, want := range tests {
		got, err := cleanAddress(in)
		if err != nil || got != want {
			t.Errorf("cleanAddress(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "1830 FITZWATER ST\x00", "<b>", "1830 FITZWATER ST;"} {
		if _, err := cleanAddress(bad); !errors.Is(err, errBadAddress) {
			t.Errorf("cleanAddress(%q) = %v, want errBadAddress", bad, err)
		}
	}
}

func TestSlugify(t *testing.T) {
	tests := map[string]string{
		"1830 Fitzwater St":   "1830-fitzwater-st",
		"  582 Paoli Ave.  ":  "582-paoli-ave",
		"2714 S. Marvine St#": "2714-s-marvine-st",
		"":                    "",
	}
	for in, want := range tests {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

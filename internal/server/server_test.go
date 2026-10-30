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
	address  string
	now      time.Time
}

func newFixture(t *testing.T, opts ...func(*Config)) *fixture {
	t.Helper()
	alarm := -4 * time.Hour
	dir := t.TempDir()
	f := &fixture{
		t:        t,
		fetcher:  newFakeFetcher(),
		holidays: filepath.Join(dir, "holidays.txt"),
		address:  filepath.Join(dir, "address.txt"),
		now:      time.Date(2026, 10, 30, 12, 0, 0, 0, philly),
	}
	cfg := Config{
		Fetcher:      f.fetcher,
		HolidaysFile: f.holidays,
		AddressFile:  f.address,
		Alarm:        &alarm,
		Refresh:      6 * time.Hour,
		Location:     philly,
		Now:          func() time.Time { return f.now },
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	f.svc = New(cfg)
	f.writeHolidays("2026-11-11 Veterans Day\n2026-11-26 Thanksgiving Day\n")
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

func (f *fixture) putHome(address string) *httptest.ResponseRecorder {
	f.t.Helper()
	body, err := json.Marshal(homeJSON{Address: address})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.do(http.MethodPut, "/api/home", "application/json", string(body))
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
	if !slices.Equal(got.HolidayYears, []int{2026}) {
		t.Errorf("holiday_years = %v, want [2026]", got.HolidayYears)
	}
	wantHolidays := []holidayJSON{{"2026-11-11", "Veterans Day"}, {"2026-11-26", "Thanksgiving Day"}}
	if !slices.Equal(got.Holidays, wantHolidays) {
		t.Errorf("holidays = %v, want %v", got.Holidays, wantHolidays)
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

func TestHomeAddress(t *testing.T) {
	f := newFixture(t)

	if got := decode[homeJSON](t, f.get("/api/home")); got.Address != "" || !got.Saving {
		t.Errorf("home before saving = %+v, want none with saving on", got)
	}

	rec := f.putHome("1830 fitzwater street")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/home = %d: %s", rec.Code, rec.Body)
	}
	if got := decode[homeJSON](t, rec); got.Address != "1830 FITZWATER ST" {
		t.Errorf("saved %q, want the city's form of the address", got.Address)
	}
	if b, err := os.ReadFile(f.address); err != nil || string(b) != "1830 FITZWATER ST\n" {
		t.Errorf("address file = %q, %v", b, err)
	}

	// Requests without an address use the home address.
	if got := decode[scheduleJSON](t, f.get("/schedule.json")); got.Address != "1830 FITZWATER ST" {
		t.Errorf("schedule without an address is for %q, want the home address", got.Address)
	}
	if rec := f.get("/schedule.ics"); !strings.Contains(rec.Body.String(), "1830-fitzwater-st@phila-trashday") {
		t.Errorf("feed without an address isn't the home address's: %d", rec.Code)
	}

	// A new service reads it back.
	again := New(Config{Fetcher: f.fetcher, AddressFile: f.address})
	if err := again.LoadHome(); err != nil || again.Home() != "1830 FITZWATER ST" {
		t.Errorf("LoadHome() = %v, home %q", err, again.Home())
	}

	rec = f.do(http.MethodDelete, "/api/home", "", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /api/home = %d: %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(f.address); !os.IsNotExist(err) {
		t.Errorf("address file still there after forgetting: %v", err)
	}
	if rec := f.get("/schedule.json"); rec.Code != http.StatusBadRequest {
		t.Errorf("schedule without an address after forgetting = %d, want 400", rec.Code)
	}
	if rec := f.do(http.MethodDelete, "/api/home", "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("forgetting twice = %d, want 204", rec.Code)
	}
}

func TestSaveHomeErrors(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name        string
		contentType string
		body        string
		status      int
	}{
		{"form post", "application/x-www-form-urlencoded", "address=1830+FITZWATER+ST", http.StatusUnsupportedMediaType},
		{"bad JSON", "application/json", "{", http.StatusBadRequest},
		{"too big", "application/json", `{"address":"` + strings.Repeat("A", 2000) + `"}`, http.StatusBadRequest},
		{"bad address", "application/json", `{"address":"<script>"}`, http.StatusBadRequest},
		{"unknown address", "application/json", `{"address":"1 NOWHERE ST"}`, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := f.do(http.MethodPut, "/api/home", tt.contentType, tt.body); rec.Code != tt.status {
				t.Errorf("PUT = %d %s, want %d", rec.Code, rec.Body, tt.status)
			}
		})
	}
	if f.svc.Home() != "" {
		t.Errorf("home = %q after failed saves, want none", f.svc.Home())
	}
}

func TestSaveHomeWriteFailures(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		f := newFixture(t, func(c *Config) { c.AddressFile = filepath.Join(t.TempDir(), "missing", "address.txt") })
		rec := f.putHome("1830 FITZWATER ST")
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "writable") {
			t.Errorf("PUT = %d %s, want 500 explaining the directory", rec.Code, rec.Body)
		}
	})

	t.Run("can't replace", func(t *testing.T) {
		// A non-empty directory where the file should be can be neither
		// renamed over nor removed.
		dir := filepath.Join(t.TempDir(), "address.txt")
		if err := os.MkdirAll(filepath.Join(dir, "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		f := newFixture(t, func(c *Config) { c.AddressFile = dir })
		if rec := f.putHome("1830 FITZWATER ST"); rec.Code != http.StatusInternalServerError {
			t.Errorf("PUT = %d, want 500", rec.Code)
		}
		if rec := f.do(http.MethodDelete, "/api/home", "", ""); rec.Code != http.StatusInternalServerError {
			t.Errorf("DELETE = %d, want 500", rec.Code)
		}
		if err := f.svc.LoadHome(); err == nil {
			t.Error("LoadHome() of a directory succeeded")
		}
	})
}

func TestSavingTurnedOff(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.AddressFile = "" })
	if got := decode[homeJSON](t, f.get("/api/home")); got.Saving {
		t.Error("saving = true with no AddressFile")
	}
	if rec := f.putHome("1830 FITZWATER ST"); rec.Code != http.StatusNotImplemented {
		t.Errorf("PUT = %d, want 501", rec.Code)
	}
	if rec := f.do(http.MethodDelete, "/api/home", "", ""); rec.Code != http.StatusNotImplemented {
		t.Errorf("DELETE = %d, want 501", rec.Code)
	}
	if err := f.svc.LoadHome(); err != nil {
		t.Errorf("LoadHome() = %v, want nothing to load", err)
	}
}

func TestLoadHome(t *testing.T) {
	dir := t.TempDir()
	missing := New(Config{AddressFile: filepath.Join(dir, "none.txt")})
	if err := missing.LoadHome(); err != nil || missing.Home() != "" {
		t.Errorf("missing file: %v, home %q", err, missing.Home())
	}

	bad := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(bad, []byte("<script>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New(Config{AddressFile: bad}).LoadHome(); err == nil || !strings.Contains(err.Error(), "bad.txt") {
		t.Errorf("bad file: %v, want an error naming it", err)
	}
}

func TestHealth(t *testing.T) {
	f := newFixture(t)
	if rec := f.get("/healthz"); rec.Code != http.StatusOK || decode[healthJSON](t, rec).Status != "ok" {
		t.Errorf("health = %d %s, want ok", rec.Code, rec.Body)
	}

	// From two months before 2027, a missing 2027 is a warning.
	f.now = time.Date(2026, 11, 15, 12, 0, 0, 0, philly)
	rec := f.get("/healthz")
	h := decode[healthJSON](t, rec)
	if rec.Code != http.StatusOK || h.Status != "degraded" || !hasWarning(h.Warnings, "no holidays listed for 2027") {
		t.Errorf("health = %d %+v, want degraded about 2027", rec.Code, h)
	}
	if got := f.schedule("1830 FITZWATER ST"); !hasWarning(got.Warnings, "2027") {
		t.Errorf("schedule warnings = %v, want the 2027 warning too", got.Warnings)
	}

	f.writeHolidays("2026-11-11\n2027-01-01 New Year's Day\n")
	if err := f.svc.ReloadHolidays(); err != nil {
		t.Fatal(err)
	}
	if h := decode[healthJSON](t, f.get("/healthz")); h.Status != "ok" {
		t.Errorf("health after adding 2027 = %+v, want ok", h)
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
		if len(got.Warnings) != 1 || !hasWarning(got.Warnings, "no holidays file") {
			t.Errorf("warnings = %v, want only the missing file", got.Warnings)
		}
		if findCollection(t, got.Collections, "2026-11-11-primary").Moved {
			t.Error("Veterans Day pickup moved without a holidays file")
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
		if !findCollection(t, got.Collections, "2026-11-11-primary").Moved {
			t.Error("Veterans Day pickup not moved, want the last good holidays kept")
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
	f.writeHolidays("2026-11-11\n2027-01-01\n")
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
		years := f.svc.holidays.Years()
		f.svc.mu.RUnlock()
		if slices.Equal(years, []int{2026, 2027}) {
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
	site := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("the page"))
	})
	f := newFixture(t, func(c *Config) { c.Site = site })
	if body := f.get("/").Body.String(); body != "the page" {
		t.Errorf("GET / = %q, want the site", body)
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
	}
	for in, want := range tests {
		got, err := ParseAlarm(in)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || *got != want {
			t.Errorf("ParseAlarm(%q) = %v, want %v", in, got, want)
		}
	}
	if got, err := ParseAlarm("OFF"); err != nil || got != nil {
		t.Errorf("ParseAlarm(OFF) = %v, %v, want no alarm", got, err)
	}
	if _, err := ParseAlarm("8pm"); err == nil {
		t.Error("ParseAlarm(8pm) succeeded")
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

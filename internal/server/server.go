// Package server serves Philadelphia trash and recycling schedules for any
// address, as JSON and as iCalendar feeds. Addresses are looked up on
// request and remembered for a while, the city's holidays come from a file
// that's re-read in the background, and one home address can be saved to a
// file and used when a request doesn't name one.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zackwag/phila-trashday/internal/ais"
	"github.com/zackwag/phila-trashday/internal/ical"
	"github.com/zackwag/phila-trashday/internal/schedule"
)

// The window of collections served: a week of past pickups, so a subscriber
// doesn't see one disappear the moment it's over, and a year ahead.
const (
	DaysBack  = 7
	DaysAhead = 365
)

const (
	// holidayWarningDays is how far ahead a year with no holidays listed
	// raises a warning. The window always reaches into next year, so warning
	// about it all year would make the warning meaningless. Two months gives
	// time to add next year's holidays before the first one.
	holidayWarningDays = 60

	// retryAfter is how long a failed lookup is remembered before the city
	// is asked again, so a mistyped address or an outage doesn't send a
	// request to the city for every page load.
	retryAfter = 5 * time.Minute

	// lookupTimeout bounds one request to the city.
	lookupTimeout = 30 * time.Second

	// maxAddressLen is the longest address accepted. Real ones are under 50
	// characters.
	maxAddressLen = 100

	// defaultMaxAddresses is how many addresses are remembered when
	// Config.MaxAddresses is zero.
	defaultMaxAddresses = 1000
)

// Fetcher looks up addresses. *ais.Client is one.
type Fetcher interface {
	Lookup(ctx context.Context, address string) (ais.Property, error)
}

// Config configures a Service.
type Config struct {
	// Fetcher looks addresses up.
	Fetcher Fetcher
	// HolidaysFile is re-read every Refresh, so edits take effect without a
	// restart. A missing file means no holidays.
	HolidaysFile string
	// AddressFile is where the home address is saved. Empty turns saving
	// off.
	AddressFile string
	// Alarm is the feed's alert, relative to the start of the pickup day,
	// when the feed URL doesn't choose one. Nil means no alert.
	Alarm *time.Duration
	// Refresh is how long a lookup is reused, how often the holidays file is
	// re-read, and how often the feed asks subscribers to fetch it again.
	Refresh time.Duration
	// MaxAddresses is how many addresses' lookups are remembered. The least
	// recently used one is forgotten first. Zero means 1000.
	MaxAddresses int
	// Location is the city's time zone, which decides what today is. Nil
	// means UTC.
	Location *time.Location
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
	// Logger receives lookup failures and holiday reloads. Nil discards
	// them.
	Logger *slog.Logger
	// Site serves every GET request the API doesn't, like the web page. Nil
	// means those get a 404.
	Site http.Handler
}

// Service holds the holidays and remembered lookups, and serves schedules.
type Service struct {
	cfg Config

	mu              sync.RWMutex
	holidays        schedule.Holidays
	holidaysErr     error
	holidaysMissing bool

	lookupsMu sync.Mutex
	lookups   map[string]*lookup

	// homeMu guards home and serializes writes to cfg.AddressFile.
	homeMu sync.Mutex
	home   string
}

// lookup is what's known about one address. Its mu is held while the city
// is asked, so concurrent requests for an address share one request.
type lookup struct {
	mu sync.Mutex
	// property is nil until a lookup succeeds.
	property  *ais.Property
	fetchedAt time.Time
	// err is the last attempt's error, errAt when attempts started failing,
	// and triedAt when the last one was made.
	err     error
	errAt   time.Time
	triedAt time.Time

	// usedAt is guarded by Service.lookupsMu, not mu.
	usedAt time.Time
}

// New returns a Service with no holidays or home address loaded. Call
// ReloadHolidays or Run, and LoadHome, to load them.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.MaxAddresses <= 0 {
		cfg.MaxAddresses = defaultMaxAddresses
	}
	return &Service{
		cfg:      cfg,
		holidays: schedule.Holidays{},
		lookups:  map[string]*lookup{},
	}
}

// Run re-reads the holidays file now and then every cfg.Refresh until ctx
// is done.
func (s *Service) Run(ctx context.Context) {
	for {
		if err := s.ReloadHolidays(); err != nil {
			s.cfg.Logger.Warn("holidays not reloaded", "err", err)
		}

		timer := time.NewTimer(s.cfg.Refresh)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// ReloadHolidays re-reads the holidays file. When it can't be read, the
// last good copy stays in use and the failure is reported as a warning until
// a later reload succeeds.
func (s *Service) ReloadHolidays() error {
	holidays, missing, err := s.loadHolidays()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.holidaysErr = err
	s.holidaysMissing = missing
	if err == nil {
		s.holidays = holidays
	}
	return err
}

// loadHolidays reads the holidays file, reporting a missing file separately
// from one that can't be read.
func (s *Service) loadHolidays() (holidays schedule.Holidays, missing bool, err error) {
	f, err := os.Open(s.cfg.HolidaysFile)
	if errors.Is(err, fs.ErrNotExist) {
		return schedule.Holidays{}, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open holidays file: %w", err)
	}
	defer func() { _ = f.Close() }()

	holidays, err = schedule.ParseHolidays(f)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", s.cfg.HolidaysFile, err)
	}
	return holidays, false, nil
}

// errNoSaving means cfg.AddressFile is empty.
var errNoSaving = errors.New("saving a home address is turned off")

// LoadHome reads the saved home address. A missing file means none is
// saved.
func (s *Service) LoadHome() error {
	if s.cfg.AddressFile == "" {
		return nil
	}
	b, err := os.ReadFile(s.cfg.AddressFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read home address: %w", err)
	}
	address, err := cleanAddress(string(b))
	if err != nil {
		return fmt.Errorf("%s: %w", s.cfg.AddressFile, err)
	}

	s.homeMu.Lock()
	defer s.homeMu.Unlock()
	s.home = address
	return nil
}

// Home returns the saved home address, or "" if none is saved.
func (s *Service) Home() string {
	s.homeMu.Lock()
	defer s.homeMu.Unlock()
	return s.home
}

// setHome saves address as the home address, or forgets it if address is
// empty. The file is written next to its final name and renamed into
// place, so a crash can't leave half an address in it. homeMu serializes
// writers, so one temporary name is enough.
func (s *Service) setHome(address string) error {
	if s.cfg.AddressFile == "" {
		return errNoSaving
	}
	s.homeMu.Lock()
	defer s.homeMu.Unlock()

	if address == "" {
		if err := os.Remove(s.cfg.AddressFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("forget home address: %w", err)
		}
		s.home = ""
		return nil
	}

	tmp := s.cfg.AddressFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(address+"\n"), 0o644); err != nil {
		return fmt.Errorf("save home address: %w", err)
	}
	if err := os.Rename(tmp, s.cfg.AddressFile); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("save home address: %w", err)
	}
	s.home = address
	return nil
}

// errBadAddress is wrapped by the errors cleanAddress returns.
var errBadAddress = errors.New("bad address")

// cleanAddress checks an address from a request and returns it the way AIS
// and the lookup cache expect it: upper case, with single spaces.
func cleanAddress(address string) (string, error) {
	address = strings.Join(strings.Fields(strings.ToUpper(address)), " ")
	switch {
	case address == "":
		return "", fmt.Errorf("%w: no address given and no home address saved; add ?address= with a Philadelphia address, like 1830 FITZWATER ST", errBadAddress)
	case len(address) > maxAddressLen:
		return "", fmt.Errorf("%w: addresses can be at most %d characters", errBadAddress, maxAddressLen)
	}
	for _, r := range address {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune(" .,'#-/&", r) {
			return "", fmt.Errorf("%w: %q can't be part of an address", errBadAddress, r)
		}
	}
	return address, nil
}

// resolved is an address's lookup as of one request.
type resolved struct {
	property  ais.Property
	fetchedAt time.Time
	// warning is set when an older lookup is served because newer ones
	// fail.
	warning string
}

// resolve returns address's pickup days, asking the city when the
// remembered lookup is too old. address has been through cleanAddress.
func (s *Service) resolve(ctx context.Context, address string) (resolved, error) {
	l := s.lookupFor(address)
	l.mu.Lock()
	defer l.mu.Unlock()

	now := s.cfg.Now()
	fresh := l.property != nil && now.Sub(l.fetchedAt) < s.cfg.Refresh
	recentlyFailed := l.err != nil && now.Sub(l.triedAt) < retryAfter
	if !fresh && !recentlyFailed {
		// A visitor closing the page shouldn't count as the city failing,
		// so the request to the city outlives the visitor's request.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lookupTimeout)
		p, err := s.cfg.Fetcher.Lookup(fetchCtx, address)
		cancel()

		l.triedAt = now
		if err == nil {
			l.property, l.fetchedAt, l.err = &p, now, nil
		} else {
			if l.err == nil {
				l.errAt = now
			}
			l.err = err
			if !errors.Is(err, ais.ErrNotFound) {
				s.cfg.Logger.Warn("address lookup failed", "address", address, "err", err)
			}
		}
	}

	if l.property == nil {
		return resolved{}, l.err
	}
	r := resolved{property: *l.property, fetchedAt: l.fetchedAt}
	if l.err != nil {
		r.warning = fmt.Sprintf("the city's address lookup has failed since %s (%v), so these pickup days are from %s",
			l.errAt.Format(time.RFC3339), l.err, l.fetchedAt.Format(time.RFC3339))
	}
	return r, nil
}

// lookupFor returns the remembered lookup for address, making one if
// needed, and forgets the least recently used address when there are too
// many.
func (s *Service) lookupFor(address string) *lookup {
	s.lookupsMu.Lock()
	defer s.lookupsMu.Unlock()

	l, ok := s.lookups[address]
	if !ok {
		if len(s.lookups) >= s.cfg.MaxAddresses {
			var oldest string
			for a, other := range s.lookups {
				if oldest == "" || other.usedAt.Before(s.lookups[oldest].usedAt) {
					oldest = a
				}
			}
			delete(s.lookups, oldest)
		}
		l = &lookup{}
		s.lookups[address] = l
	}
	l.usedAt = s.cfg.Now()
	return l
}

// view is one address's schedule as of one request.
type view struct {
	resolved
	generated   time.Time
	years       []int
	holidays    []schedule.Holiday
	collections []schedule.Collection
	warnings    []string
}

func (s *Service) view(r resolved) view {
	now := s.cfg.Now()
	today := schedule.Day(now.In(s.cfg.Location))
	from, to := today.AddDate(0, 0, -DaysBack), today.AddDate(0, 0, DaysAhead)

	s.mu.RLock()
	defer s.mu.RUnlock()
	v := view{
		resolved:    r,
		generated:   now,
		years:       s.holidays.Years(),
		holidays:    s.holidays.Between(from, to),
		collections: schedule.Build(r.property.Days, s.holidays, from, to),
		warnings:    s.holidayWarnings(today),
	}
	if r.warning != "" {
		v.warnings = append(v.warnings, r.warning)
	}
	return v
}

// holidayWarnings lists what's wrong with the holidays. The caller holds
// s.mu.
func (s *Service) holidayWarnings(today time.Time) []string {
	var w []string
	switch {
	case s.holidaysErr != nil:
		w = append(w, fmt.Sprintf("can't read holidays (%v), so the last good copy is in use", s.holidaysErr))
	case s.holidaysMissing:
		return append(w, fmt.Sprintf("no holidays file at %s, so no pickups are moved for holidays", s.cfg.HolidaysFile))
	}

	years := s.holidays.Years()
	for y := today.Year(); y <= today.AddDate(0, 0, holidayWarningDays).Year(); y++ {
		if !slices.Contains(years, y) {
			w = append(w, fmt.Sprintf("no holidays listed for %d, so pickups that year aren't moved for holidays", y))
		}
	}
	return w
}

// ParseAlarm turns a time the evening before a pickup into an offset from
// the start of the pickup day, so "20:00" is -4h. "off" means no alarm.
func ParseAlarm(s string) (*time.Duration, error) {
	if strings.EqualFold(s, "off") {
		return nil, nil
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return nil, fmt.Errorf("alarm must be HH:MM, like 20:00, or off, not %q", s)
	}
	offset := time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute - 24*time.Hour
	return &offset, nil
}

// Handler serves /schedule.json, /schedule.ics, /api/home and /healthz, and
// hands other GET requests to cfg.Site.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /schedule.json", s.serveJSON)
	mux.HandleFunc("GET /schedule.ics", s.serveICS)
	mux.HandleFunc("GET /api/home", s.serveHome)
	mux.HandleFunc("PUT /api/home", s.saveHome)
	mux.HandleFunc("DELETE /api/home", s.forgetHome)
	mux.HandleFunc("GET /healthz", s.serveHealth)
	if s.cfg.Site != nil {
		mux.Handle("GET /", s.cfg.Site)
	}
	return mux
}

type collectionJSON struct {
	ID            string          `json:"id"`
	Date          string          `json:"date"`
	ScheduledDate string          `json:"scheduled_date"`
	Moved         bool            `json:"moved"`
	Slot          schedule.Slot   `json:"slot"`
	Kinds         []schedule.Kind `json:"kinds"`
	Summary       string          `json:"summary"`
}

type holidayJSON struct {
	Date string `json:"date"`
	Name string `json:"name"`
}

type scheduleJSON struct {
	Address       string           `json:"address"`
	PrimaryDay    string           `json:"primary_day"`
	SecondaryDay  string           `json:"secondary_day,omitempty"`
	DaysFetchedAt time.Time        `json:"days_fetched_at"`
	GeneratedAt   time.Time        `json:"generated_at"`
	HolidayYears  []int            `json:"holiday_years"`
	Holidays      []holidayJSON    `json:"holidays"`
	Warnings      []string         `json:"warnings"`
	Collections   []collectionJSON `json:"collections"`
}

type errorJSON struct {
	Error string `json:"error"`
}

type homeJSON struct {
	Address string `json:"address"`
	// Saving reports whether the home address can be changed.
	Saving bool `json:"saving"`
}

type healthJSON struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings"`
}

// lookupRequest resolves the request's address, or the home address when
// it doesn't name one, or writes the error and returns false.
func (s *Service) lookupRequest(w http.ResponseWriter, r *http.Request) (view, bool) {
	query := r.URL.Query().Get("address")
	if strings.TrimSpace(query) == "" && s.Home() != "" {
		query = s.Home()
	}
	address, err := cleanAddress(query)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, errorJSON{Error: err.Error()})
		return view{}, false
	}
	res, ok := s.resolveRequest(r.Context(), w, address)
	if !ok {
		return view{}, false
	}
	return s.view(res), true
}

// resolveRequest resolves address, or writes the error and returns false.
func (s *Service) resolveRequest(ctx context.Context, w http.ResponseWriter, address string) (resolved, bool) {
	res, err := s.resolve(ctx, address)
	switch {
	case errors.Is(err, ais.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, errorJSON{
			Error: fmt.Sprintf("the city has no property at %s; write it the way the city does, like 1830 FITZWATER ST", address),
		})
		return resolved{}, false
	case err != nil:
		s.writeJSON(w, http.StatusBadGateway, errorJSON{
			Error: fmt.Sprintf("the city's address lookup didn't answer (%v); try again in a few minutes", err),
		})
		return resolved{}, false
	}
	return res, true
}

func (s *Service) serveHome(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, homeJSON{Address: s.Home(), Saving: s.cfg.AddressFile != ""})
}

// saveHome saves the address in the body as the home address, after
// checking the city knows it. Requiring PUT with a JSON body means a
// browser asks this server's permission first (a CORS preflight, which it
// never grants), so another site can't change the address from a visitor's
// browser.
func (s *Service) saveHome(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AddressFile == "" {
		s.writeJSON(w, http.StatusNotImplemented, errorJSON{Error: errNoSaving.Error()})
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		s.writeJSON(w, http.StatusUnsupportedMediaType, errorJSON{Error: "send the address as JSON, like {\"address\": \"1830 FITZWATER ST\"}"})
		return
	}
	var body homeJSON
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errorJSON{Error: "send the address as JSON, like {\"address\": \"1830 FITZWATER ST\"}"})
		return
	}
	address, err := cleanAddress(body.Address)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, errorJSON{Error: err.Error()})
		return
	}
	res, ok := s.resolveRequest(r.Context(), w, address)
	if !ok {
		return
	}

	// Save the city's form of the address, so the feed's event IDs match
	// whichever way it was typed.
	if err := s.setHome(res.property.Address); err != nil {
		s.cfg.Logger.Warn("save home address", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorJSON{
			Error: fmt.Sprintf("%v; the directory holding %s has to be writable by the container's user", err, s.cfg.AddressFile),
		})
		return
	}
	s.writeJSON(w, http.StatusOK, homeJSON{Address: res.property.Address, Saving: true})
}

func (s *Service) forgetHome(w http.ResponseWriter, _ *http.Request) {
	if err := s.setHome(""); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errNoSaving) {
			status = http.StatusNotImplemented
		}
		s.writeJSON(w, status, errorJSON{Error: err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) serveJSON(w http.ResponseWriter, r *http.Request) {
	v, ok := s.lookupRequest(w, r)
	if !ok {
		return
	}

	days := v.property.Days
	out := scheduleJSON{
		Address:       v.property.Address,
		PrimaryDay:    days.Primary.String(),
		DaysFetchedAt: v.fetchedAt.UTC(),
		GeneratedAt:   v.generated.UTC(),
		HolidayYears:  orEmpty(v.years),
		Holidays:      make([]holidayJSON, len(v.holidays)),
		Warnings:      orEmpty(v.warnings),
		Collections:   make([]collectionJSON, len(v.collections)),
	}
	if days.HasSecondary {
		out.SecondaryDay = days.Secondary.String()
	}
	for i, h := range v.holidays {
		out.Holidays[i] = holidayJSON{Date: h.Date.Format(schedule.DateLayout), Name: h.Name}
	}
	for i, c := range v.collections {
		out.Collections[i] = collectionJSON{
			ID:            c.ID,
			Date:          c.Date.Format(schedule.DateLayout),
			ScheduledDate: c.Scheduled.Format(schedule.DateLayout),
			Moved:         c.Moved(),
			Slot:          c.Slot,
			Kinds:         c.Kinds,
			Summary:       c.Summary(),
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Service) serveICS(w http.ResponseWriter, r *http.Request) {
	alarm := s.cfg.Alarm
	if r.URL.Query().Has("alarm") {
		var err error
		if alarm, err = ParseAlarm(r.URL.Query().Get("alarm")); err != nil {
			s.writeJSON(w, http.StatusBadRequest, errorJSON{Error: err.Error()})
			return
		}
	}
	v, ok := s.lookupRequest(w, r)
	if !ok {
		return
	}

	cal := ical.Calendar{
		ProdID:  "-//zackwag//phila-trashday//EN",
		Name:    "Trash and recycling",
		Refresh: s.cfg.Refresh,
		Events:  make([]ical.Event, len(v.collections)),
	}
	slug := slugify(v.property.Address)
	for i, c := range v.collections {
		e := ical.Event{
			UID:     c.ID + "." + slug + "@phila-trashday",
			Date:    c.Date,
			Summary: c.Summary(),
			Alarm:   alarm,
		}
		if c.Moved() {
			e.Description = "Moved from " + c.Scheduled.Format("Monday, January 2") + " for a holiday."
		}
		cal.Events[i] = e
	}

	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	if err := ical.Write(w, cal, v.generated); err != nil {
		s.cfg.Logger.Warn("write calendar", "err", err)
	}
}

func (s *Service) serveHealth(w http.ResponseWriter, _ *http.Request) {
	today := schedule.Day(s.cfg.Now().In(s.cfg.Location))
	s.mu.RLock()
	warnings := s.holidayWarnings(today)
	s.mu.RUnlock()

	if len(warnings) > 0 {
		s.writeJSON(w, http.StatusOK, healthJSON{Status: "degraded", Warnings: warnings})
		return
	}
	s.writeJSON(w, http.StatusOK, healthJSON{Status: "ok", Warnings: []string{}})
}

func (s *Service) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(body); err != nil {
		s.cfg.Logger.Warn("write response", "err", err)
	}
}

// orEmpty returns s, or an empty slice if it's nil, so JSON gets [] rather
// than null.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// slugify turns an address into something safe for a UID, like
// "1830-fitzwater-st".
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

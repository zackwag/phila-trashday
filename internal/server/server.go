// Package server serves Philadelphia trash and recycling schedules for any
// address, as JSON and as iCalendar feeds. Addresses are looked up on
// request and remembered for a while. The city's holidays are computed, and
// a file of extra ones is re-read in the background. Addresses can be saved under
// random IDs, each with its own stable URLs, and one of them is the default
// for requests that don't name an address.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"uuid"

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
	// HolidaysFile lists extra holidays, on top of the city's own, like a
	// closure the mayor announces. It's re-read every Refresh, so edits take
	// effect without a restart. It's optional.
	HolidaysFile string
	// AddressesFile is where saved addresses are kept. Empty turns saving
	// off.
	AddressesFile string
	// PublicURL is where a reverse proxy serves saved addresses' feeds from
	// outside the network, without a trailing slash, like
	// https://example.com/trashday. The page shows it so calendar services
	// like iCloud and Skylight can subscribe. Empty means the feeds aren't
	// public.
	PublicURL string
	// Alarm is the feed's alert, relative to the start of the pickup day,
	// when the feed URL doesn't choose one. Nil means no alert.
	Alarm *time.Duration
	// Refresh is how long a lookup is reused, how often the extra holidays
	// file is re-read, and how often the feed asks subscribers to fetch it
	// again.
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

// Service holds the extra holidays and remembered lookups, and serves
// schedules.
type Service struct {
	cfg Config

	mu          sync.RWMutex
	extra       schedule.Holidays
	holidaysErr error

	lookupsMu sync.Mutex
	lookups   map[string]*lookup

	// savedMu guards saved and defaultID, and serializes writes to
	// cfg.AddressesFile.
	savedMu   sync.Mutex
	saved     []Saved
	defaultID uuid.UUID
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

// New returns a Service with no extra holidays or saved addresses loaded.
// Call ReloadHolidays or Run, and LoadSaved, to load them.
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
		cfg:     cfg,
		extra:   schedule.Holidays{},
		lookups: map[string]*lookup{},
	}
}

// Run re-reads the extra holidays file now and then every cfg.Refresh until
// ctx is done.
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

// ReloadHolidays re-reads the extra holidays file. When it can't be read,
// the last good copy stays in use and the failure is reported as a warning
// until a later reload succeeds.
func (s *Service) ReloadHolidays() error {
	extra, err := s.loadHolidays()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.holidaysErr = err
	if err == nil {
		s.extra = extra
	}
	return err
}

// loadHolidays reads the extra holidays file. A missing file means none.
func (s *Service) loadHolidays() (schedule.Holidays, error) {
	f, err := os.Open(s.cfg.HolidaysFile)
	if errors.Is(err, fs.ErrNotExist) {
		return schedule.Holidays{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open holidays file: %w", err)
	}
	defer func() { _ = f.Close() }()

	extra, err := schedule.ParseHolidays(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.cfg.HolidaysFile, err)
	}
	return extra, nil
}

// holidays returns the city's holidays for every year from..to touches, plus
// the extra ones. Pickups are built from the week before from, so the year
// before is included too. The caller holds s.mu.
func (s *Service) holidays(from, to time.Time) schedule.Holidays {
	all := schedule.Holidays{}
	for year := from.Year() - 1; year <= to.Year(); year++ {
		maps.Copy(all, schedule.CityHolidays(year))
	}
	maps.Copy(all, s.extra)
	return all
}

// errBadAddress is wrapped by the errors cleanAddress returns.
var errBadAddress = errors.New("bad address")

// cleanAddress checks an address from a request and returns it the way AIS
// and the lookup cache expect it: upper case, with single spaces.
func cleanAddress(address string) (string, error) {
	address = strings.Join(strings.Fields(strings.ToUpper(address)), " ")
	switch {
	case address == "":
		return "", fmt.Errorf("%w: no address given and no default address saved; add ?address= with a Philadelphia address, like 1830 FITZWATER ST", errBadAddress)
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
	// saved is the saved address the schedule is for, if it's saved.
	saved          *Saved
	savedIsDefault bool
	generated      time.Time
	holidays       []schedule.Holiday
	collections    []schedule.Collection
	warnings       []string
}

func (s *Service) view(r resolved) view {
	now := s.cfg.Now()
	today := schedule.Day(now.In(s.cfg.Location))
	from, to := today.AddDate(0, 0, -DaysBack), today.AddDate(0, 0, DaysAhead)

	s.mu.RLock()
	defer s.mu.RUnlock()
	holidays := s.holidays(from, to)
	v := view{
		resolved:    r,
		generated:   now,
		holidays:    holidays.Between(from, to),
		collections: schedule.Build(r.property.Days, holidays, from, to),
		warnings:    s.holidayWarnings(),
	}
	if r.warning != "" {
		v.warnings = append(v.warnings, r.warning)
	}
	return v
}

// holidayWarnings lists what's wrong with the extra holidays file. The
// caller holds s.mu.
func (s *Service) holidayWarnings() []string {
	if s.holidaysErr != nil {
		return []string{fmt.Sprintf("can't read the extra holidays file (%v), so its last good copy is in use", s.holidaysErr)}
	}
	return nil
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

// Handler serves the schedules (/schedule.json and /schedule.ics, and the
// same under /a/{id}/ for a saved address), /api/addresses and /healthz,
// and hands other GET requests to cfg.Site.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /schedule.json", s.serveJSON)
	mux.HandleFunc("GET /schedule.ics", s.serveICS)
	mux.HandleFunc("GET /a/{id}/schedule.json", s.serveJSON)
	mux.HandleFunc("GET /a/{id}/schedule.ics", s.serveICS)
	mux.HandleFunc("GET /api/addresses", s.listSaved)
	mux.HandleFunc("POST /api/addresses", s.addSaved)
	mux.HandleFunc("PATCH /api/addresses/{id}", s.updateSaved)
	mux.HandleFunc("DELETE /api/addresses/{id}", s.removeSaved)
	mux.HandleFunc("GET /healthz", s.serveHealth)
	if s.cfg.Site != nil {
		mux.Handle("GET /", s.cfg.Site)
		// A saved address's page is the same page, which reads the ID
		// from the URL.
		mux.HandleFunc("GET /a/{id}", func(w http.ResponseWriter, r *http.Request) {
			page := r.Clone(r.Context())
			page.URL.Path, page.URL.RawPath = "/", ""
			s.cfg.Site.ServeHTTP(w, page)
		})
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

// scheduleSavedJSON says which saved address a schedule is for.
type scheduleSavedJSON struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
}

type scheduleJSON struct {
	Address string `json:"address"`
	// Saved is set when the address is saved, however it was asked for.
	Saved         *scheduleSavedJSON `json:"saved,omitempty"`
	PrimaryDay    string             `json:"primary_day"`
	SecondaryDay  string             `json:"secondary_day,omitempty"`
	DaysFetchedAt time.Time          `json:"days_fetched_at"`
	GeneratedAt   time.Time          `json:"generated_at"`
	Holidays      []holidayJSON      `json:"holidays"`
	Warnings      []string           `json:"warnings"`
	Collections   []collectionJSON   `json:"collections"`
}

type errorJSON struct {
	Error string `json:"error"`
}

type healthJSON struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings"`
}

// lookupRequest resolves the address a request is for: the saved address
// in its /a/{id}/ path, its ?address=, or the default saved address. It
// writes the error and returns false if it can't.
func (s *Service) lookupRequest(w http.ResponseWriter, r *http.Request) (view, bool) {
	query := r.URL.Query().Get("address")
	if id := r.PathValue("id"); id != "" {
		saved, _, ok := s.savedByID(id)
		if !ok {
			s.writeSavedError(w, errNoSuchSaved)
			return view{}, false
		}
		query = saved.Address
	} else if strings.TrimSpace(query) == "" {
		if saved, _, ok := s.findSaved(func(a Saved) bool { return a.ID == s.defaultSavedID() }); ok {
			query = saved.Address
		}
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
	v := s.view(res)
	if saved, isDefault, ok := s.findSaved(func(a Saved) bool { return a.Address == res.property.Address }); ok {
		v.saved, v.savedIsDefault = &saved, isDefault
	}
	return v, true
}

// defaultSavedID returns the default saved address's ID, or the nil UUID.
func (s *Service) defaultSavedID() uuid.UUID {
	_, def := s.savedList()
	return def
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
		Holidays:      make([]holidayJSON, len(v.holidays)),
		Warnings:      orEmpty(v.warnings),
		Collections:   make([]collectionJSON, len(v.collections)),
	}
	if days.HasSecondary {
		out.SecondaryDay = days.Secondary.String()
	}
	if v.saved != nil {
		out.Saved = &scheduleSavedJSON{ID: v.saved.ID.String(), Label: v.saved.Label, Default: v.savedIsDefault}
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

	name := "Trash and recycling"
	if v.saved != nil && v.saved.Label != "" {
		name += " (" + v.saved.Label + ")"
	}
	cal := ical.Calendar{
		ProdID:  "-//zackwag//phila-trashday//EN",
		Name:    name,
		Refresh: s.cfg.Refresh,
		Events:  make([]ical.Event, len(v.collections)),
	}
	// A saved address's events are named by its ID, so the feed, which
	// calendar services fetch, doesn't spell out the address.
	owner := slugify(v.property.Address)
	if v.saved != nil {
		owner = v.saved.ID.String()
	}
	for i, c := range v.collections {
		e := ical.Event{
			UID:     c.ID + "." + owner + "@phila-trashday",
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
	s.mu.RLock()
	warnings := s.holidayWarnings()
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

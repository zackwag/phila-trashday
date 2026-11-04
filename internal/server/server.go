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
	"slices"
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

// DefaultAlarm is the reminder a feed gets when neither its saved address
// nor its URL chooses one: 8 PM the evening before.
const DefaultAlarm = "20:00"

// normalizeAlarm checks a reminder setting, the time the evening before a
// pickup as HH:MM or "off", and writes it the one way it's stored.
func normalizeAlarm(s string) (string, error) {
	if strings.EqualFold(s, "off") {
		return "off", nil
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return "", fmt.Errorf("alarm must be HH:MM, like 20:00, or off, not %q", s)
	}
	return t.Format("15:04"), nil
}

// parseAlarm turns a reminder setting into an offset from the start of the
// pickup day, so "20:00" is -4h. "off" means no reminder.
func parseAlarm(s string) (*time.Duration, error) {
	s, err := normalizeAlarm(s)
	if err != nil || s == "off" {
		return nil, err
	}
	t, _ := time.Parse("15:04", s) // normalizeAlarm has checked it
	offset := time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute - 24*time.Hour
	return &offset, nil
}

// alarmSetting is the reminder a saved address's feed gets.
func alarmSetting(a Saved) string {
	if a.Alarm == "" {
		return DefaultAlarm
	}
	return a.Alarm
}

// Handler serves the schedules (/schedule.json and /schedule.ics, and the
// same under /a/{id}/ for a saved address), /api/addresses and /healthz,
// and hands other GET requests to cfg.Site.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /schedule.json", s.serveJSON)
	mux.HandleFunc("GET /schedule.ics", s.serveICS)
	mux.HandleFunc("GET /a/lookup", s.serveLookup)
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
	s.cfg.Logger.Info("schedule", "address", v.property.Address, "format", "json", "path", r.URL.Path)

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

// serveLookup resolves an address, saves it if it isn't already, and
// redirects to its feed page. This lets someone on the preview page look
// up their own address and get their own subscribable link without
// exposing the full app.
func (s *Service) serveLookup(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AddressesFile == "" {
		s.writeJSON(w, http.StatusNotImplemented, errorJSON{Error: "saving addresses is turned off"})
		return
	}
	query := r.URL.Query().Get("address")
	if strings.TrimSpace(query) == "" {
		s.serveLookupError(w, r, "")
		return
	}
	address, err := cleanAddress(query)
	if err != nil {
		s.serveLookupError(w, r, err.Error())
		return
	}
	res, rErr := s.resolve(r.Context(), address)
	if rErr != nil {
		msg := fmt.Sprintf("No property found at %s. Write it the way the city does, like 1830 FITZWATER ST.", address)
		if !errors.Is(rErr, ais.ErrNotFound) {
			msg = "The city's address lookup didn't answer. Try again in a few minutes."
		}
		s.serveLookupError(w, r, msg)
		return
	}

	// Save the address, or find the existing one.
	var id uuid.UUID
	existing, _, ok := s.findSaved(func(a Saved) bool { return a.Address == res.property.Address })
	if ok {
		id = existing.ID
	} else {
		var added Saved
		err := s.change(func(saved []Saved, def uuid.UUID) ([]Saved, uuid.UUID, error) {
			// Re-check under the lock.
			if i := slices.IndexFunc(saved, func(a Saved) bool { return a.Address == res.property.Address }); i >= 0 {
				id = saved[i].ID
				return nil, def, errAlreadySaved
			}
			if len(saved) >= maxSaved {
				return nil, def, errTooManySaved
			}
			added = Saved{ID: uuid.NewV4(), Address: res.property.Address, CreatedAt: s.cfg.Now().UTC()}
			if def == uuid.Nil() {
				def = added.ID
			}
			return append(saved, added), def, nil
		})
		switch {
		case errors.Is(err, errAlreadySaved):
			// id was set inside the lock.
		case err != nil:
			s.serveLookupError(w, r, "Couldn't save that address. Try again.")
			return
		default:
			id = added.ID
		}
	}

	s.cfg.Logger.Info("lookup", "address", res.property.Address, "saved_id", id.String(), "new", !ok)
	// Use the public URL when set, since a path-stripping reverse proxy
	// makes Go's relative redirect resolve to the wrong absolute path.
	target := "/a/" + id.String() + "/schedule.ics"
	if s.cfg.PublicURL != "" {
		target = s.cfg.PublicURL + target
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// serveLookupError renders the preview page's search form with an error
// message, so the user can try again without losing context.
func (s *Service) serveLookupError(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src https://phl.carto.com; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	var b strings.Builder
	writeLookupPage(&b, r.URL.Query().Get("address"), msg)
	_, _ = fmt.Fprint(w, b.String())
}

// wantsHTML reports whether the client prefers HTML over a calendar feed.
// Only browsers send Accept: text/html; calendar clients ask for
// text/calendar or */*. Defaulting to the feed keeps Skylight and every
// other subscriber working without knowing their exact Accept header.
func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html") && !strings.Contains(accept, "text/calendar")
}

// feedPagePickups is how many upcoming collections the feed preview page
// shows.
const feedPagePickups = 6

// alarmOptions are the times the preview page's reminder picker offers.
var alarmOptions = []struct{ Value, Label string }{
	{"18:00", "6:00 PM"},
	{"19:00", "7:00 PM"},
	{"20:00", "8:00 PM"},
	{"21:00", "9:00 PM"},
	{"22:00", "10:00 PM"},
	{"off", "Don’t remind me"},
}

// serveFeedPage renders a read-only HTML preview of a saved address's
// schedule, with a reminder picker and a webcal:// subscribe button.
func (s *Service) serveFeedPage(w http.ResponseWriter, r *http.Request, v view) {
	// The alarm for the subscribe link: URL param wins, then saved setting.
	alarm := DefaultAlarm
	if v.saved != nil {
		alarm = alarmSetting(*v.saved)
	}
	if r.URL.Query().Has("alarm") {
		if a, err := normalizeAlarm(r.URL.Query().Get("alarm")); err == nil {
			alarm = a
		}
	}

	// Build the webcal:// subscribe URL with the alarm baked in.
	subURL := *r.URL
	subURL.Scheme = "webcal"
	subURL.Host = r.Host
	q := subURL.Query()
	q.Set("alarm", alarm)
	subURL.RawQuery = q.Encode()

	today := schedule.Day(s.cfg.Now().In(s.cfg.Location))
	upcoming := v.collections
	// Trim past pickups.
	cut := 0
	for cut < len(upcoming) && upcoming[cut].Date.Before(today) {
		cut++
	}
	upcoming = upcoming[cut:]
	if len(upcoming) > feedPagePickups {
		upcoming = upcoming[:feedPagePickups]
	}

	label := v.property.Address
	if v.saved != nil && v.saved.Label != "" {
		label = v.saved.Label
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src https://phl.carto.com; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")

	var b strings.Builder
	feedPageHead(&b, label+" — Trash night")
	b.WriteString(`<div class="card">
<h1>`)
	htmlEscape(&b, label)
	b.WriteString(`</h1>
<p class="addr">`)
	htmlEscape(&b, v.property.Address)
	b.WriteString(`</p>
<ul class="pickups">
`)
	for _, c := range upcoming {
		b.WriteString(`<li class="pickup"><span class="pickup-when">`)
		htmlEscape(&b, c.Date.Format("Mon, Jan 2"))
		b.WriteString(`</span><span class="pickup-what">`)
		htmlEscape(&b, c.Summary())
		b.WriteString(`</span></li>
`)
		if c.Moved() {
			b.WriteString(`<li class="moved">Moved from `)
			htmlEscape(&b, c.Scheduled.Format("Mon, Jan 2"))
			b.WriteString(` for a holiday</li>
`)
		}
	}
	b.WriteString(`</ul>
<p class="note">Subscribe once and every pickup shows up, including the ones holidays move.</p>
<div class="field">
<label for="alarm">Remind me the night before at</label>
<select id="alarm">
`)
	for _, o := range alarmOptions {
		b.WriteString(`<option value="`)
		htmlEscape(&b, o.Value)
		b.WriteByte('"')
		if o.Value == alarm {
			b.WriteString(` selected`)
		}
		b.WriteByte('>')
		htmlEscape(&b, o.Label)
		b.WriteString("</option>\n")
	}
	b.WriteString(`</select>
</div>
<a class="sub" id="sub" href="`)
	htmlEscape(&b, subURL.String())
	b.WriteString(`">Subscribe in your calendar</a>
<script>
document.getElementById('alarm').addEventListener('change',function(){
  var a=document.getElementById('sub'),u=new URL(a.href);
  u.searchParams.set('alarm',this.value);a.href=u.toString();
});
</script>
</div>
`)
	writeLookupForm(&b, "../lookup", "", "")
	b.WriteString(`</body>
</html>
`)
	_, _ = fmt.Fprint(w, b.String())
}

// feedPageHead writes the shared <head> and opening <body> for the
// preview and lookup pages.
func feedPageHead(b *strings.Builder, title string) {
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>`)
	htmlEscape(b, title)
	b.WriteString(`</title>
<style>
*{box-sizing:border-box;margin:0}
body{font:400 1.125rem/1.55 system-ui,sans-serif;color:#1e2328;background:#eef0f1;padding:2rem clamp(1.25rem,5vw,2.5rem)}
@media(prefers-color-scheme:dark){body{color:#e6eaee;background:#161b20}a{color:#6c9ce6}.card{background:#1f262d;border-color:#333c45}.pickup{border-color:#333c45}input{color:#e6eaee;background:#1f262d;border-color:#333c45}}
.card{max-width:36rem;background:#fff;border:1px solid #d5dade;border-radius:0.625rem;padding:1.5rem 1.75rem;margin-bottom:1.25rem}
h1{font-size:1.5rem;font-weight:800;letter-spacing:-0.01em}
h2{font-size:1.125rem;font-weight:700;margin-bottom:0.5rem}
.addr{color:#545e67;margin-top:0.25rem}
.pickups{list-style:none;padding:0;margin:1.25rem 0 0}
.pickup{display:flex;justify-content:space-between;align-items:center;padding:0.6rem 0;border-top:1px solid #d5dade}
.pickup-when{font-weight:700}
.pickup-what{color:#545e67}
.moved{font-size:0.875rem;color:#545e67}
.note{color:#545e67;margin-top:1rem}
.field{display:flex;flex-wrap:wrap;align-items:center;gap:0.5rem 0.75rem;margin-top:0.75rem}
.field select{font:inherit;font-weight:600;padding:0.45rem 0.75rem 0.3rem;color:#1e2328;background:#fff;border:2px solid #d5dade;border-radius:0.625rem}
.sub,.btn{display:inline-block;margin-top:1rem;padding:0.65rem 1.25rem;font:inherit;font-weight:700;color:#eef0f1;background:#1e2328;border:2px solid #1e2328;border-radius:0.625rem;text-decoration:none;cursor:pointer}
.sub:hover,.btn:hover{background:#1f5fbf;border-color:#1f5fbf}
@media(prefers-color-scheme:dark){.sub,.btn{color:#161b20;background:#e6eaee;border-color:#e6eaee}.sub:hover,.btn:hover{background:#6c9ce6;border-color:#6c9ce6}.field select{color:#e6eaee;background:#1f262d;border-color:#333c45}}
.lookup{display:flex;align-items:center;gap:0.5rem;flex-wrap:wrap}
.combo{position:relative;flex:1 1 14rem}
.combo input{width:100%;font:inherit;padding:0.65rem 0.85rem;border:2px solid #d5dade;border-radius:0.625rem;background:#fff}
.combo input:focus{outline:none;border-color:#1f5fbf}
.suggestions{position:absolute;z-index:2;inset:calc(100% + 0.375rem) 0 auto;margin:0;padding:0.375rem;list-style:none;background:#fff;border:1px solid #d5dade;border-radius:0.625rem;box-shadow:0 12px 24px -12px rgb(14 26 46/0.35)}
.suggestions li{padding:0.55rem 0.75rem;border-radius:0.375rem;cursor:pointer}
.suggestions li[aria-selected="true"]{background:color-mix(in srgb,#1f5fbf 14%,transparent)}
.error{color:#b42318;font-weight:600;margin-bottom:0.75rem}
@media(prefers-color-scheme:dark){.error{color:#f97066}.combo input{color:#e6eaee;background:#1f262d;border-color:#333c45}.suggestions{background:#1f262d;border-color:#333c45}}
</style>
</head>
<body>
`)
}

// writeLookupForm writes the "Look up your address" form with Carto
// autocomplete, and an optional prefilled value and error message.
// action is the form's action URL, relative to the page serving it.
func writeLookupForm(b *strings.Builder, action, value, errMsg string) {
	b.WriteString(`<div class="card">
<h2>Look up your address</h2>
`)
	if errMsg != "" {
		b.WriteString(`<p class="error">`)
		htmlEscape(b, errMsg)
		b.WriteString(`</p>
`)
	}
	b.WriteString(`<form class="lookup" id="lookup" action="`)
	htmlEscape(b, action)
	b.WriteString(`" method="get" autocomplete="off">
<div class="combo">
<input id="addr" name="address" type="text" inputmode="search" spellcheck="false"
  placeholder="1830 Fitzwater St" role="combobox" aria-autocomplete="list"
  aria-expanded="false" aria-controls="sug" required value="`)
	htmlEscape(b, value)
	b.WriteString(`">
<ul class="suggestions" id="sug" role="listbox" hidden></ul>
</div>
<button class="btn" type="submit">Show schedule</button>
</form>
</div>
<script>
(function(){
var TYPES={street:'ST',avenue:'AVE',boulevard:'BLVD',drive:'DR',road:'RD',
  lane:'LN',court:'CT',place:'PL',circle:'CIR',terrace:'TER',
  parkway:'PKWY',square:'SQ',trail:'TRL'};
var DIRS={north:'N',south:'S',east:'E',west:'W',
  northeast:'NE',northwest:'NW',southeast:'SE',southwest:'SW'};
function norm(s){
  return s.trim().replace(/\./g,'').split(/\s+/).map(function(w,i){
    var l=w.toLowerCase();
    if(DIRS[l])return DIRS[l];
    if(TYPES[l]&&i>0)return TYPES[l];
    var m=w.match(/^(\d+)(st|nd|rd|th)$/i);
    if(m&&i>0)return m[1]+m[2].toUpperCase();
    return w.toUpperCase();
  }).join(' ');
}
function display(a){
  return a.split(' ').map(function(w){
    if(/^(N|S|E|W|NE|NW|SE|SW)$/.test(w))return w;
    if(/^\d+(ST|ND|RD|TH)$/.test(w))return w.toLowerCase();
    return w.charAt(0)+w.slice(1).toLowerCase();
  }).join(' ');
}
function prefixes(q){
  var words=q.trim().split(/\s+/),ps=[norm(words.join(' '))];
  var last=words[words.length-1].toLowerCase();
  if(words.length>2&&last.length>=3){
    var k=Object.keys(TYPES).find(function(t){return t.indexOf(last)===0;});
    if(k){var ab=norm(words.slice(0,-1).concat(TYPES[k]).join(' '));if(ab!==ps[0])ps.push(ab);}
  }
  return ps.map(function(p){return p.replace(/[^A-Z0-9 ,'#/&-]/g,'');}).filter(function(p){return p.length>=3;});
}
var input=document.getElementById('addr'),list=document.getElementById('sug');
var opts=[],active=-1,timer=0,seq=0;
function close(){list.hidden=true;input.setAttribute('aria-expanded','false');active=-1;}
function hi(i){
  active=i;
  [].forEach.call(list.children,function(li,j){li.setAttribute('aria-selected',String(j===i));});
}
function choose(a){input.value=display(a);close();document.getElementById('lookup').submit();}
function open(addrs){
  opts=addrs;list.innerHTML='';
  addrs.forEach(function(a,i){
    var li=document.createElement('li');li.setAttribute('role','option');
    li.setAttribute('aria-selected','false');li.textContent=display(a);
    li.onmousedown=function(e){e.preventDefault();choose(a);};
    li.onmousemove=function(){hi(i);};
    list.appendChild(li);
  });
  if(!addrs.length){close();return;}
  list.hidden=false;input.setAttribute('aria-expanded','true');hi(-1);
}
input.addEventListener('input',function(){
  clearTimeout(timer);
  timer=setTimeout(function(){
    var ps=prefixes(input.value);if(!ps.length){close();return;}
    var where=ps.map(function(p){return"UPPER(location) LIKE '"+p.replace(/'/g,"''")+"%%'";}).join(' OR ');
    var s=++seq;
    fetch('https://phl.carto.com/api/v2/sql?q='+encodeURIComponent(
      'SELECT DISTINCT location FROM opa_properties_public WHERE '+where+' ORDER BY location LIMIT 8'
    )).then(function(r){return r.json();}).then(function(d){
      if(s===seq)open((d.rows||[]).map(function(r){return r.location;}).filter(Boolean));
    }).catch(function(){});
  },250);
});
input.addEventListener('keydown',function(e){
  if(list.hidden)return;
  if(e.key==='ArrowDown'){e.preventDefault();hi(Math.min(active+1,opts.length-1));}
  else if(e.key==='ArrowUp'){e.preventDefault();hi(Math.max(active-1,0));}
  else if(e.key==='Enter'&&active>=0){e.preventDefault();choose(opts[active]);}
  else if(e.key==='Escape')close();
});
input.addEventListener('blur',close);
})();
</script>
`)
}

// writeLookupPage writes a standalone page with just the lookup form.
func writeLookupPage(b *strings.Builder, value, errMsg string) {
	feedPageHead(b, "Trash night")
	writeLookupForm(b, "lookup", value, errMsg)
	b.WriteString(`</body>
</html>
`)
}

// htmlEscape writes s to b with HTML-unsafe characters escaped.
func htmlEscape(b *strings.Builder, s string) {
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#39;")
		default:
			b.WriteRune(r)
		}
	}
}

func (s *Service) serveICS(w http.ResponseWriter, r *http.Request) {
	v, ok := s.lookupRequest(w, r)
	if !ok {
		return
	}
	// A saved address's .ics URL doubles as a shareable link: browsers get
	// an HTML preview with a subscribe button, calendar clients get the feed.
	if r.PathValue("id") != "" && wantsHTML(r) {
		s.cfg.Logger.Info("schedule", "address", v.property.Address, "format", "preview", "path", r.URL.Path)
		s.serveFeedPage(w, r, v)
		return
	}
	s.cfg.Logger.Info("schedule", "address", v.property.Address, "format", "ics", "path", r.URL.Path)
	// The URL's reminder wins, then the saved address's. Leaving it out of
	// a saved address's URL means changing it reaches calendars that
	// already subscribe.
	setting := DefaultAlarm
	if v.saved != nil {
		setting = alarmSetting(*v.saved)
	}
	if r.URL.Query().Has("alarm") {
		setting = r.URL.Query().Get("alarm")
	}
	alarm, err := parseAlarm(setting)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, errorJSON{Error: err.Error()})
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

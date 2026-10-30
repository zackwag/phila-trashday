package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/zackwag/phila-trashday/internal/ais"
	"github.com/zackwag/phila-trashday/internal/schedule"
)

func (f *fixture) list() savedListJSON {
	f.t.Helper()
	return decode[savedListJSON](f.t, f.get("/api/addresses"))
}

func (f *fixture) mustSave(address, label string) savedJSON {
	f.t.Helper()
	rec := f.save(address, label)
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("saving %q = %d: %s", address, rec.Code, rec.Body)
	}
	return decode[savedJSON](f.t, rec)
}

func (f *fixture) patch(id, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(http.MethodPatch, "/api/addresses/"+id, "application/json", body)
}

func (f *fixture) remove(id string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(http.MethodDelete, "/api/addresses/"+id, "", "")
}

func (f *fixture) savedFile() savedFile {
	f.t.Helper()
	b, err := os.ReadFile(f.saved)
	if err != nil {
		f.t.Fatal(err)
	}
	var out savedFile
	if err := json.Unmarshal(b, &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func TestSaveAndList(t *testing.T) {
	f := newFixture(t)
	if got := f.list(); len(got.Addresses) != 0 || !got.Saving {
		t.Fatalf("list before saving = %+v, want none with saving on", got)
	}

	home := f.mustSave("1830 fitzwater street", "  Home  ")
	if _, err := uuid.Parse(home.ID); err != nil {
		t.Errorf("ID %q isn't a UUID: %v", home.ID, err)
	}
	if home.Address != "1830 FITZWATER ST" || home.Label != "Home" || !home.Default ||
		home.Feed != "/a/"+home.ID+"/schedule.ics" || !home.CreatedAt.Equal(f.now) {
		t.Errorf("saved = %+v", home)
	}

	other := f.mustSave("582 PAOLI AVE", "")
	if other.Default || other.Label != "" {
		t.Errorf("second address = %+v, want it not the default and unlabeled", other)
	}

	// Saving an address that's already saved, however it's typed, returns
	// the saved one.
	rec := f.save("1830 FITZWATER STREET", "Something else")
	if rec.Code != http.StatusOK || decode[savedJSON](t, rec).ID != home.ID {
		t.Errorf("saving again = %d %s, want 200 with the existing address", rec.Code, rec.Body)
	}

	list := f.list()
	if len(list.Addresses) != 2 || list.Addresses[0].ID != home.ID || list.Addresses[1].ID != other.ID {
		t.Errorf("list = %+v, want the two addresses, oldest first", list.Addresses)
	}
	file := f.savedFile()
	if file.Default.String() != home.ID || len(file.Addresses) != 2 || file.Addresses[0].Label != "Home" {
		t.Errorf("file = %+v", file)
	}

	// A new address can be made the default as it's saved.
	third := f.mustSave("2714 S MARVINE ST", "Rental")
	if third.Default {
		t.Error("third address became the default without asking")
	}
	rec = f.do(http.MethodPost, "/api/addresses", "application/json", `{"address": "9868 COWDEN ST", "default": true}`)
	if got := decode[savedJSON](t, rec); rec.Code != http.StatusCreated || !got.Default {
		t.Errorf("saving with default = %d %+v, want 201 and the default", rec.Code, got)
	}
}

func TestSavedAddressURLs(t *testing.T) {
	f := newFixture(t)
	home := f.mustSave("1830 FITZWATER ST", "Home")
	paoli := f.mustSave("582 PAOLI AVE", "")

	got := decode[scheduleJSON](t, f.get("/a/"+home.ID+"/schedule.json"))
	if got.Address != "1830 FITZWATER ST" || got.Saved == nil || got.Saved.ID != home.ID || got.Saved.Label != "Home" || !got.Saved.Default {
		t.Errorf("schedule by ID = address %q, saved %+v", got.Address, got.Saved)
	}

	// The same address asked for by name still says it's saved.
	if got := f.schedule("1830 fitzwater street"); got.Saved == nil || got.Saved.ID != home.ID {
		t.Errorf("schedule by address saved = %+v, want the saved address", got.Saved)
	}
	if got := f.schedule("2714 S MARVINE ST"); got.Saved != nil {
		t.Errorf("unsaved address has saved = %+v", got.Saved)
	}

	// No address means the default.
	if got := decode[scheduleJSON](t, f.get("/schedule.json")); got.Address != "1830 FITZWATER ST" {
		t.Errorf("schedule without an address is for %q, want the default", got.Address)
	}

	ics := f.get("/a/" + home.ID + "/schedule.ics?alarm=21:00").Body.String()
	for _, want := range []string{"X-WR-CALNAME:Trash and recycling (Home)", "." + home.ID + "@phila-trashday", "TRIGGER:-PT3H"} {
		if !strings.Contains(ics, want) {
			t.Errorf("feed by ID is missing %q", want)
		}
	}
	// Calendar services fetch the feed, so a saved address's feed doesn't
	// spell out the address, even when it's asked for by address.
	for _, path := range []string{"/a/" + home.ID + "/schedule.ics", "/schedule.ics?address=1830+FITZWATER+ST", "/schedule.ics"} {
		if body := f.get(path).Body.String(); strings.Contains(body, "fitzwater") || strings.Contains(body, "FITZWATER") {
			t.Errorf("%s mentions the address", path)
		}
	}
	if ics := f.get("/a/" + paoli.ID + "/schedule.ics").Body.String(); !strings.Contains(ics, "X-WR-CALNAME:Trash and recycling\r\n") {
		t.Error("unlabeled address's feed name isn't the plain one")
	}

	for _, path := range []string{
		"/a/" + uuid.NewV4().String() + "/schedule.json",
		"/a/not-a-uuid/schedule.ics",
	} {
		if rec := f.get(path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestPublicFeed(t *testing.T) {
	f := newFixture(t)
	if got := f.mustSave("1830 FITZWATER ST", ""); got.PublicFeed != "" {
		t.Errorf("public_feed = %q without PublicURL, want none", got.PublicFeed)
	}

	f = newFixture(t, func(c *Config) { c.PublicURL = "https://example.com/trashday" })
	home := f.mustSave("1830 FITZWATER ST", "Home")
	want := "https://example.com/trashday/a/" + home.ID + "/schedule.ics"
	if home.PublicFeed != want {
		t.Errorf("public_feed = %q, want %q", home.PublicFeed, want)
	}
	list := f.list()
	if got := list.Addresses[0].PublicFeed; got != want {
		t.Errorf("listed public_feed = %q, want %q", got, want)
	}
	if list.PublicURL != "https://example.com/trashday" {
		t.Errorf("public_url = %q", list.PublicURL)
	}
}

func TestUpdateSaved(t *testing.T) {
	f := newFixture(t)
	home := f.mustSave("1830 FITZWATER ST", "Home")
	paoli := f.mustSave("582 PAOLI AVE", "")

	rec := f.patch(paoli.ID, `{"label": "Mom's", "default": true}`)
	got := decode[savedJSON](t, rec)
	if rec.Code != http.StatusOK || got.Label != "Mom's" || !got.Default || got.ID != paoli.ID {
		t.Fatalf("PATCH = %d %+v", rec.Code, got)
	}
	if list := f.list(); list.Addresses[0].Default {
		t.Error("old default still the default")
	}

	// Changing the address keeps the ID, so subscribed feeds follow it.
	rec = f.patch(home.ID, `{"address": "2714 s marvine st"}`)
	if got := decode[savedJSON](t, rec); rec.Code != http.StatusOK || got.Address != "2714 S MARVINE ST" || got.Label != "Home" {
		t.Errorf("moving the address = %d %+v", rec.Code, got)
	}
	if got := decode[scheduleJSON](t, f.get("/a/"+home.ID+"/schedule.json")); got.Address != "2714 S MARVINE ST" {
		t.Errorf("feed after moving is for %q", got.Address)
	}

	// Clearing the default leaves none.
	if rec := f.patch(paoli.ID, `{"default": false}`); decode[savedJSON](t, rec).Default {
		t.Error("default: false didn't clear it")
	}
	if rec := f.get("/schedule.json"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no default address") {
		t.Errorf("schedule without an address or default = %d %s", rec.Code, rec.Body)
	}
	// Clearing it on an address that isn't the default changes nothing.
	f.patch(home.ID, `{"default": true}`)
	f.patch(paoli.ID, `{"default": false}`)
	if list := f.list(); !list.Addresses[0].Default {
		t.Error("clearing the default on another address cleared the real one")
	}

	tests := []struct {
		name, id, body string
		status         int
	}{
		{"address saved under another ID", paoli.ID, `{"address": "2714 S MARVINE ST"}`, http.StatusConflict},
		{"unknown ID", uuid.NewV4().String(), `{"label": "x"}`, http.StatusNotFound},
		{"label too long", home.ID, `{"label": "` + strings.Repeat("x", maxLabelLen+1) + `"}`, http.StatusBadRequest},
		{"control character in label", home.ID, `{"label": "a\u0007b"}`, http.StatusBadRequest},
		{"bad address", home.ID, `{"address": "<b>"}`, http.StatusBadRequest},
		{"unknown address", home.ID, `{"address": "1 NOWHERE ST"}`, http.StatusNotFound},
		{"unknown field", home.ID, `{"lable": "Home"}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := f.patch(tt.id, tt.body); rec.Code != tt.status {
				t.Errorf("PATCH = %d %s, want %d", rec.Code, rec.Body, tt.status)
			}
		})
	}
}

func TestUpdateSavedRemovedMeanwhile(t *testing.T) {
	f := newFixture(t)
	home := f.mustSave("1830 FITZWATER ST", "Home")

	// The address is removed while the new address is being checked with
	// the city.
	f.fetcher.mu.Lock()
	f.fetcher.hook = func(address string) {
		if address == "582 PAOLI AVE" {
			f.remove(home.ID)
		}
	}
	f.fetcher.mu.Unlock()
	if rec := f.patch(home.ID, `{"address": "582 PAOLI AVE"}`); rec.Code != http.StatusNotFound {
		t.Errorf("PATCH of an address removed meanwhile = %d, want 404", rec.Code)
	}
}

func TestRemoveSaved(t *testing.T) {
	f := newFixture(t)
	home := f.mustSave("1830 FITZWATER ST", "Home")
	paoli := f.mustSave("582 PAOLI AVE", "")
	marvine := f.mustSave("2714 S MARVINE ST", "")

	if rec := f.remove(home.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", rec.Code, rec.Body)
	}
	list := f.list()
	if len(list.Addresses) != 2 || !list.Addresses[0].Default || list.Addresses[0].ID != paoli.ID {
		t.Errorf("after removing the default = %+v, want the oldest left as the default", list.Addresses)
	}
	if rec := f.get("/a/" + home.ID + "/schedule.ics"); rec.Code != http.StatusNotFound {
		t.Errorf("removed address's feed = %d, want 404", rec.Code)
	}

	f.remove(marvine.ID)
	if list := f.list(); len(list.Addresses) != 1 || !list.Addresses[0].Default {
		t.Errorf("removing a non-default changed the default: %+v", list.Addresses)
	}
	f.remove(paoli.ID)
	if file := f.savedFile(); len(file.Addresses) != 0 || file.Default != uuid.Nil() {
		t.Errorf("file after removing everything = %+v", file)
	}

	if rec := f.remove(home.ID); rec.Code != http.StatusNotFound {
		t.Errorf("removing twice = %d, want 404", rec.Code)
	}
}

func TestSaveErrors(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name, contentType, body string
		status                  int
	}{
		{"form post", "application/x-www-form-urlencoded", "address=1830+FITZWATER+ST", http.StatusUnsupportedMediaType},
		{"bad JSON", "application/json", "{", http.StatusBadRequest},
		{"too big", "application/json", `{"address":"` + strings.Repeat("A", 2000) + `"}`, http.StatusBadRequest},
		{"no address", "application/json", `{"label": "Home"}`, http.StatusBadRequest},
		{"bad address", "application/json", `{"address": "<script>"}`, http.StatusBadRequest},
		{"bad label", "application/json", `{"address": "1830 FITZWATER ST", "label": "a\u0000"}`, http.StatusBadRequest},
		{"unknown address", "application/json", `{"address": "1 NOWHERE ST"}`, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec := f.do(http.MethodPost, "/api/addresses", tt.contentType, tt.body); rec.Code != tt.status {
				t.Errorf("POST = %d %s, want %d", rec.Code, rec.Body, tt.status)
			}
		})
	}
	if list := f.list(); len(list.Addresses) != 0 {
		t.Errorf("failed saves saved %+v", list.Addresses)
	}
}

func TestTooManySaved(t *testing.T) {
	f := newFixture(t)
	f.fetcher.mu.Lock()
	for i := range maxSaved + 1 {
		address := fmt.Sprintf("%d TEST ST", i+1)
		f.fetcher.known[address] = ais.Property{Address: address, Days: schedule.Days{Primary: time.Monday}}
	}
	f.fetcher.mu.Unlock()

	for i := range maxSaved {
		f.mustSave(fmt.Sprintf("%d TEST ST", i+1), "")
	}
	if rec := f.save(fmt.Sprintf("%d TEST ST", maxSaved+1), ""); rec.Code != http.StatusConflict {
		t.Errorf("saving one too many = %d %s, want 409", rec.Code, rec.Body)
	}
}

func TestSavingTurnedOff(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.AddressesFile = "" })
	if got := f.list(); got.Saving {
		t.Error("saving = true with no AddressesFile")
	}
	if rec := f.save("1830 FITZWATER ST", ""); rec.Code != http.StatusNotImplemented {
		t.Errorf("POST = %d, want 501", rec.Code)
	}
	if err := f.svc.LoadSaved(); err != nil {
		t.Errorf("LoadSaved() = %v, want nothing to load", err)
	}
	// With nothing saved, these find nothing before saving would matter.
	if rec := f.remove(uuid.NewV4().String()); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE = %d, want 404", rec.Code)
	}
}

func TestSaveWriteFailures(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		f := newFixture(t, func(c *Config) { c.AddressesFile = filepath.Join(t.TempDir(), "missing", "addresses.json") })
		rec := f.save("1830 FITZWATER ST", "")
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "writable") {
			t.Errorf("POST = %d %s, want 500 explaining the directory", rec.Code, rec.Body)
		}
		if list := f.list(); len(list.Addresses) != 0 {
			t.Errorf("a save that couldn't be written is listed: %+v", list.Addresses)
		}
	})

	t.Run("can't replace", func(t *testing.T) {
		f := newFixture(t)
		home := f.mustSave("1830 FITZWATER ST", "Home")
		// A non-empty directory where the file should be can't be renamed
		// over.
		if err := os.Remove(f.saved); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(f.saved, "child"), 0o700); err != nil {
			t.Fatal(err)
		}
		if rec := f.remove(home.ID); rec.Code != http.StatusInternalServerError {
			t.Errorf("DELETE = %d, want 500", rec.Code)
		}
		if list := f.list(); len(list.Addresses) != 1 {
			t.Errorf("a removal that couldn't be written took effect: %+v", list.Addresses)
		}
		if rec := f.patch(home.ID, `{"label": "x"}`); rec.Code != http.StatusInternalServerError {
			t.Errorf("PATCH = %d, want 500", rec.Code)
		}
	})
}

func TestLoadSaved(t *testing.T) {
	f := newFixture(t)
	home := f.mustSave("1830 FITZWATER ST", "Home")
	f.mustSave("582 PAOLI AVE", "")

	again := New(Config{Fetcher: f.fetcher, AddressesFile: f.saved})
	if err := again.LoadSaved(); err != nil {
		t.Fatal(err)
	}
	list, def := again.savedList()
	if len(list) != 2 || list[0].ID.String() != home.ID || def.String() != home.ID || list[0].Label != "Home" {
		t.Errorf("loaded %+v, default %v", list, def)
	}

	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "addresses.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("default that isn't saved", func(t *testing.T) {
		path := write(t, fmt.Sprintf(`{"default": %q, "addresses": [{"id": %q, "address": "1830 FITZWATER ST"}]}`,
			uuid.NewV4(), uuid.NewV4()))
		svc := New(Config{AddressesFile: path})
		if err := svc.LoadSaved(); err != nil {
			t.Fatal(err)
		}
		if _, def := svc.savedList(); def != uuid.Nil() {
			t.Errorf("default = %v, want none", def)
		}
	})

	for name, content := range map[string]string{
		"not JSON":    "{",
		"missing ID":  `{"addresses": [{"address": "1830 FITZWATER ST"}]}`,
		"bad address": fmt.Sprintf(`{"addresses": [{"id": %q, "address": "<b>"}]}`, uuid.NewV4()),
	} {
		t.Run(name, func(t *testing.T) {
			if err := New(Config{AddressesFile: write(t, content)}).LoadSaved(); err == nil || !strings.Contains(err.Error(), "addresses.json") {
				t.Errorf("LoadSaved() = %v, want an error naming the file", err)
			}
		})
	}

	t.Run("no file yet", func(t *testing.T) {
		svc := New(Config{AddressesFile: filepath.Join(t.TempDir(), "addresses.json")})
		if err := svc.LoadSaved(); err != nil {
			t.Fatal(err)
		}
		if saved, def := svc.savedList(); len(saved) != 0 || def != uuid.Nil() {
			t.Errorf("loaded %+v, default %v, want nothing", saved, def)
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		if err := New(Config{AddressesFile: t.TempDir()}).LoadSaved(); err == nil {
			t.Error("LoadSaved() of a directory succeeded")
		}
	})
}

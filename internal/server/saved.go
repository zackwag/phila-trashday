package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"uuid"
)

const (
	// maxSaved is how many addresses can be saved. A household needs a
	// handful; the limit keeps a runaway client from growing the file
	// without bound.
	maxSaved = 100

	// maxLabelLen is the longest label a saved address can have, in
	// characters.
	maxLabelLen = 40
)

// Saved is an address saved on the server under a random ID. Its feed URLs
// use the ID, so they keep working when its label or even its address
// changes, and they don't reveal the address.
type Saved struct {
	ID        uuid.UUID `json:"id"`
	Address   string    `json:"address"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// savedFile is the format of Config.AddressesFile.
type savedFile struct {
	// Default is the address used when a request names none, or the nil
	// UUID for none.
	Default   uuid.UUID `json:"default"`
	Addresses []Saved   `json:"addresses"`
}

var (
	errNoSaving     = errors.New("saving addresses is turned off")
	errNoSuchSaved  = errors.New("no saved address has that ID")
	errAlreadySaved = errors.New("that address is already saved")
	errTooManySaved = fmt.Errorf("at most %d addresses can be saved", maxSaved)
	errBadLabel     = errors.New("bad label")
)

// LoadSaved reads the saved addresses. A missing file means none are saved.
func (s *Service) LoadSaved() error {
	if s.cfg.AddressesFile == "" {
		return nil
	}
	b, err := os.ReadFile(s.cfg.AddressesFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read saved addresses: %w", err)
	}

	var f savedFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("%s: %w", s.cfg.AddressesFile, err)
	}
	for _, a := range f.Addresses {
		if a.ID == uuid.Nil() {
			return fmt.Errorf("%s: an address has no ID", s.cfg.AddressesFile)
		}
		if _, err := cleanAddress(a.Address); err != nil {
			return fmt.Errorf("%s: %w", s.cfg.AddressesFile, err)
		}
	}
	if !slices.ContainsFunc(f.Addresses, func(a Saved) bool { return a.ID == f.Default }) {
		f.Default = uuid.Nil()
	}

	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	s.saved, s.defaultID = f.Addresses, f.Default
	return nil
}

// persist writes the saved addresses next to the file and renames them into
// place, so a crash can't leave a half-written file. The caller holds
// savedMu, which also makes one temporary name enough.
func (s *Service) persist() error {
	// Marshaling these types can't fail.
	b, _ := json.MarshalIndent(savedFile{Default: s.defaultID, Addresses: orEmpty(s.saved)}, "", "  ")
	tmp := s.cfg.AddressesFile + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("save addresses: %w", err)
	}
	if err := os.Rename(tmp, s.cfg.AddressesFile); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("save addresses: %w", err)
	}
	return nil
}

// change edits a copy of the saved addresses with edit and saves the
// result. If saving fails, nothing changes. Callers check saving is on:
// with it off, nothing is ever saved for an edit to find.
func (s *Service) change(edit func(saved []Saved, def uuid.UUID) ([]Saved, uuid.UUID, error)) error {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()

	saved, def, err := edit(slices.Clone(s.saved), s.defaultID)
	if err != nil {
		return err
	}
	oldSaved, oldDef := s.saved, s.defaultID
	s.saved, s.defaultID = saved, def
	if err := s.persist(); err != nil {
		s.saved, s.defaultID = oldSaved, oldDef
		return err
	}
	return nil
}

// savedList returns the saved addresses, oldest first, and the default's
// ID.
func (s *Service) savedList() ([]Saved, uuid.UUID) {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	return slices.Clone(s.saved), s.defaultID
}

// findSaved returns the saved address matching, and whether it's the
// default.
func (s *Service) findSaved(match func(Saved) bool) (saved Saved, isDefault, ok bool) {
	list, def := s.savedList()
	i := slices.IndexFunc(list, match)
	if i < 0 {
		return Saved{}, false, false
	}
	return list[i], list[i].ID == def, true
}

// savedByID finds a saved address by the ID in a URL.
func (s *Service) savedByID(id string) (saved Saved, isDefault, ok bool) {
	u, err := uuid.Parse(id)
	if err != nil {
		return Saved{}, false, false
	}
	return s.findSaved(func(a Saved) bool { return a.ID == u })
}

// cleanLabel checks a label from a request and collapses its spaces.
func cleanLabel(label string) (string, error) {
	label = strings.Join(strings.Fields(label), " ")
	if utf8.RuneCountInString(label) > maxLabelLen {
		return "", fmt.Errorf("%w: labels can be at most %d characters", errBadLabel, maxLabelLen)
	}
	if strings.ContainsFunc(label, unicode.IsControl) {
		return "", fmt.Errorf("%w: labels can't have control characters", errBadLabel)
	}
	return label, nil
}

type savedJSON struct {
	ID        string    `json:"id"`
	Address   string    `json:"address"`
	Label     string    `json:"label"`
	Default   bool      `json:"default"`
	CreatedAt time.Time `json:"created_at"`
	// Feed is the path of the address's calendar feed.
	Feed string `json:"feed"`
}

func toSavedJSON(a Saved, isDefault bool) savedJSON {
	return savedJSON{
		ID:        a.ID.String(),
		Address:   a.Address,
		Label:     a.Label,
		Default:   isDefault,
		CreatedAt: a.CreatedAt,
		Feed:      "/a/" + a.ID.String() + "/schedule.ics",
	}
}

type savedListJSON struct {
	Addresses []savedJSON `json:"addresses"`
	// Saving reports whether addresses can be saved and changed.
	Saving bool `json:"saving"`
}

// savedRequest is the body of a POST or PATCH. Fields left out of a PATCH
// aren't changed.
type savedRequest struct {
	Address *string `json:"address"`
	Label   *string `json:"label"`
	Default *bool   `json:"default"`
}

func (s *Service) listSaved(w http.ResponseWriter, _ *http.Request) {
	list, def := s.savedList()
	out := savedListJSON{Addresses: make([]savedJSON, len(list)), Saving: s.cfg.AddressesFile != ""}
	for i, a := range list {
		out.Addresses[i] = toSavedJSON(a, a.ID == def)
	}
	s.writeJSON(w, http.StatusOK, out)
}

// readSavedRequest decodes a POST or PATCH body. Requiring a JSON body
// means a browser asks this server's permission before sending one from
// another site (a CORS preflight, which it never grants), so another site
// can't change the saved addresses from a visitor's browser.
func (s *Service) readSavedRequest(w http.ResponseWriter, r *http.Request) (savedRequest, bool) {
	const example = `send JSON, like {"address": "1830 FITZWATER ST", "label": "Home"}`
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		s.writeJSON(w, http.StatusUnsupportedMediaType, errorJSON{Error: example})
		return savedRequest{}, false
	}
	var body savedRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errorJSON{Error: example})
		return savedRequest{}, false
	}
	if body.Label != nil {
		label, err := cleanLabel(*body.Label)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, errorJSON{Error: err.Error()})
			return savedRequest{}, false
		}
		body.Label = &label
	}
	return body, true
}

// resolveBodyAddress checks the address in a request body with the city
// and returns the city's form of it, so an address is saved once however
// it was typed.
func (s *Service) resolveBodyAddress(ctx context.Context, w http.ResponseWriter, address string) (string, bool) {
	clean, err := cleanAddress(address)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, errorJSON{Error: err.Error()})
		return "", false
	}
	res, ok := s.resolveRequest(ctx, w, clean)
	if !ok {
		return "", false
	}
	return res.property.Address, true
}

// addSaved saves an address. Saving one that's already saved returns the
// existing one, with a 200 instead of a 201.
func (s *Service) addSaved(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AddressesFile == "" {
		s.writeSavedError(w, errNoSaving)
		return
	}
	body, ok := s.readSavedRequest(w, r)
	if !ok {
		return
	}
	if body.Address == nil {
		s.writeJSON(w, http.StatusBadRequest, errorJSON{Error: "add the address to save"})
		return
	}
	address, ok := s.resolveBodyAddress(r.Context(), w, *body.Address)
	if !ok {
		return
	}

	var added Saved
	var existing *Saved
	err := s.change(func(saved []Saved, def uuid.UUID) ([]Saved, uuid.UUID, error) {
		if i := slices.IndexFunc(saved, func(a Saved) bool { return a.Address == address }); i >= 0 {
			existing = &saved[i]
			return nil, def, errAlreadySaved
		}
		if len(saved) >= maxSaved {
			return nil, def, errTooManySaved
		}
		added = Saved{ID: uuid.NewV4(), Address: address, CreatedAt: s.cfg.Now().UTC()}
		if body.Label != nil {
			added.Label = *body.Label
		}
		if def == uuid.Nil() || (body.Default != nil && *body.Default) {
			def = added.ID
		}
		return append(saved, added), def, nil
	})
	if errors.Is(err, errAlreadySaved) {
		_, isDefault, _ := s.findSaved(func(a Saved) bool { return a.ID == existing.ID })
		s.writeJSON(w, http.StatusOK, toSavedJSON(*existing, isDefault))
		return
	}
	if err != nil {
		s.writeSavedError(w, err)
		return
	}
	_, isDefault, _ := s.findSaved(func(a Saved) bool { return a.ID == added.ID })
	s.writeJSON(w, http.StatusCreated, toSavedJSON(added, isDefault))
}

// updateSaved changes a saved address's label, address or default status.
// Its ID, and so its URLs, stay the same.
func (s *Service) updateSaved(w http.ResponseWriter, r *http.Request) {
	current, _, found := s.savedByID(r.PathValue("id"))
	if !found {
		s.writeSavedError(w, errNoSuchSaved)
		return
	}
	body, ok := s.readSavedRequest(w, r)
	if !ok {
		return
	}
	address := current.Address
	if body.Address != nil {
		if address, ok = s.resolveBodyAddress(r.Context(), w, *body.Address); !ok {
			return
		}
	}

	var updated Saved
	err := s.change(func(saved []Saved, def uuid.UUID) ([]Saved, uuid.UUID, error) {
		i := slices.IndexFunc(saved, func(a Saved) bool { return a.ID == current.ID })
		if i < 0 {
			return nil, def, errNoSuchSaved
		}
		if slices.ContainsFunc(saved, func(a Saved) bool { return a.Address == address && a.ID != current.ID }) {
			return nil, def, errAlreadySaved
		}
		saved[i].Address = address
		if body.Label != nil {
			saved[i].Label = *body.Label
		}
		switch {
		case body.Default == nil:
		case *body.Default:
			def = current.ID
		case def == current.ID:
			def = uuid.Nil()
		}
		updated = saved[i]
		return saved, def, nil
	})
	if err != nil {
		s.writeSavedError(w, err)
		return
	}
	_, isDefault, _ := s.findSaved(func(a Saved) bool { return a.ID == updated.ID })
	s.writeJSON(w, http.StatusOK, toSavedJSON(updated, isDefault))
}

// removeSaved forgets a saved address. Its URLs stop working. If it was
// the default, the oldest remaining address becomes the default.
func (s *Service) removeSaved(w http.ResponseWriter, r *http.Request) {
	current, _, found := s.savedByID(r.PathValue("id"))
	if !found {
		s.writeSavedError(w, errNoSuchSaved)
		return
	}
	err := s.change(func(saved []Saved, def uuid.UUID) ([]Saved, uuid.UUID, error) {
		saved = slices.DeleteFunc(saved, func(a Saved) bool { return a.ID == current.ID })
		if def == current.ID {
			def = uuid.Nil()
			if len(saved) > 0 {
				def = saved[0].ID
			}
		}
		return saved, def, nil
	})
	if err != nil {
		s.writeSavedError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) writeSavedError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoSaving):
		s.writeJSON(w, http.StatusNotImplemented, errorJSON{Error: err.Error()})
	case errors.Is(err, errNoSuchSaved):
		s.writeJSON(w, http.StatusNotFound, errorJSON{Error: err.Error()})
	case errors.Is(err, errAlreadySaved), errors.Is(err, errTooManySaved):
		s.writeJSON(w, http.StatusConflict, errorJSON{Error: err.Error()})
	default:
		s.cfg.Logger.Warn("save addresses", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorJSON{
			Error: fmt.Sprintf("%v; the directory holding %s has to be writable by the container's user", err, s.cfg.AddressesFile),
		})
	}
}

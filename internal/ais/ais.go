// Package ais looks up an address's trash and recycling days in
// Philadelphia's Address Information System (AIS) API.
package ais

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zackwag/phila-trashday/internal/schedule"
)

// DefaultBaseURL is the city's API host.
const DefaultBaseURL = "https://api.phila.gov"

// ErrNotFound means AIS doesn't know the address.
var ErrNotFound = errors.New("address not found in AIS")

// maxBody caps how much of a response is read. An address lookup is a few
// kilobytes.
const maxBody = 1 << 20

var weekdays = map[string]time.Weekday{
	"MON": time.Monday,
	"TUE": time.Tuesday,
	"WED": time.Wednesday,
	"THU": time.Thursday,
	"FRI": time.Friday,
	"SAT": time.Saturday,
	"SUN": time.Sunday,
}

// Client looks up addresses in AIS.
type Client struct {
	// HTTP sends the requests. Nil means http.DefaultClient.
	HTTP *http.Client
	// BaseURL is the API host, normally DefaultBaseURL.
	BaseURL string
	// UserAgent identifies this service to the city.
	UserAgent string
}

type response struct {
	Features []struct {
		Properties struct {
			StreetAddress       string  `json:"street_address"`
			RubbishRecycleDay   string  `json:"rubbish_recycle_day"`
			SecondaryRubbishDay *string `json:"secondary_rubbish_day"`
		} `json:"properties"`
	} `json:"features"`
}

// Property is an address as AIS knows it.
type Property struct {
	// Address is AIS's form of the address, like "1830 FITZWATER ST".
	Address string
	Days    schedule.Days
}

// Lookup returns the property at address, written the way phila.gov's
// collection day search shows it, like "1830 FITZWATER ST". When AIS matches
// several properties, the first one is used.
func (c *Client) Lookup(ctx context.Context, address string) (Property, error) {
	u := strings.TrimSuffix(c.BaseURL, "/") + "/ais/v1/addresses/" +
		url.PathEscape(strings.ToUpper(strings.TrimSpace(address)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Property{}, fmt.Errorf("build AIS request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Property{}, fmt.Errorf("query AIS: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return Property{}, fmt.Errorf("%q: %w", address, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return Property{}, fmt.Errorf("query AIS: %s", resp.Status)
	}

	var body response
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return Property{}, fmt.Errorf("decode AIS response: %w", err)
	}
	if len(body.Features) == 0 {
		return Property{}, fmt.Errorf("%q: %w", address, ErrNotFound)
	}

	props := body.Features[0].Properties
	primary, ok := weekdays[strings.ToUpper(props.RubbishRecycleDay)]
	if !ok {
		return Property{}, fmt.Errorf("AIS has no collection day for %s: rubbish_recycle_day is %q",
			props.StreetAddress, props.RubbishRecycleDay)
	}
	p := Property{Address: props.StreetAddress, Days: schedule.Days{Primary: primary}}
	if p.Address == "" {
		p.Address = strings.ToUpper(strings.TrimSpace(address))
	}
	if props.SecondaryRubbishDay != nil {
		if secondary, ok := weekdays[strings.ToUpper(*props.SecondaryRubbishDay)]; ok {
			p.Days.Secondary = secondary
			p.Days.HasSecondary = true
		}
	}
	return p, nil
}

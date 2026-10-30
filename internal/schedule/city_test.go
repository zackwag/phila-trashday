package schedule

import (
	"maps"
	"slices"
	"testing"
)

// cityHolidays2026 is the city's own list from api.phila.gov/phila/trashday/v1,
// fetched in a browser on 2026-10-30.
var cityHolidays2026 = map[string]string{
	"2026-01-01": "New Year's Day",
	"2026-01-19": "Martin Luther King, Jr. Day",
	"2026-02-16": "Presidents' Day",
	"2026-04-03": "Good Friday",
	"2026-05-25": "Memorial Day",
	"2026-06-19": "Juneteenth",
	"2026-07-04": "Independence Day",
	"2026-09-07": "Labor Day",
	"2026-10-12": "Columbus Day/Indigenous Peoples' Day",
	"2026-11-11": "Veterans Day",
	"2026-11-26": "Thanksgiving",
	"2026-11-27": "Thanksgiving Friday",
	"2026-12-25": "Christmas Day",
}

func formatHolidays(h Holidays) map[string]string {
	out := map[string]string{}
	for d, name := range h {
		out[d.Format(DateLayout)] = name
	}
	return out
}

func TestCityHolidaysMatchTheCity(t *testing.T) {
	got := formatHolidays(CityHolidays(2026))
	if !maps.Equal(got, cityHolidays2026) {
		for _, d := range slices.Sorted(maps.Keys(cityHolidays2026)) {
			if got[d] != cityHolidays2026[d] {
				t.Errorf("%s: got %q, want %q", d, got[d], cityHolidays2026[d])
			}
		}
		for _, d := range slices.Sorted(maps.Keys(got)) {
			if _, ok := cityHolidays2026[d]; !ok {
				t.Errorf("%s: got %q, which the city doesn't list", d, got[d])
			}
		}
	}
}

func TestCityHolidaysOtherYears(t *testing.T) {
	tests := []struct {
		year int
		want map[string]string
	}{
		{2027, map[string]string{
			"2027-01-01": "New Year's Day",
			"2027-01-18": "Martin Luther King, Jr. Day",
			"2027-02-15": "Presidents' Day",
			"2027-03-26": "Good Friday",
			"2027-05-31": "Memorial Day",
			// June 19 is a Saturday, so it stays there.
			"2027-06-19": "Juneteenth",
			// July 4 is a Sunday, so it's observed Monday.
			"2027-07-05": "Independence Day",
			"2027-09-06": "Labor Day",
			"2027-10-11": "Columbus Day/Indigenous Peoples' Day",
			"2027-11-11": "Veterans Day",
			"2027-11-25": "Thanksgiving",
			"2027-11-26": "Thanksgiving Friday",
			// December 25 is a Saturday.
			"2027-12-25": "Christmas Day",
		}},
		{2028, map[string]string{
			"2028-01-01": "New Year's Day",
			"2028-01-17": "Martin Luther King, Jr. Day",
			"2028-02-21": "Presidents' Day",
			"2028-04-14": "Good Friday",
			"2028-05-29": "Memorial Day",
			"2028-06-19": "Juneteenth",
			"2028-07-04": "Independence Day",
			"2028-09-04": "Labor Day",
			"2028-10-09": "Columbus Day/Indigenous Peoples' Day",
			"2028-11-11": "Veterans Day",
			"2028-11-23": "Thanksgiving",
			"2028-11-24": "Thanksgiving Friday",
			"2028-12-25": "Christmas Day",
		}},
	}
	for _, tt := range tests {
		if got := formatHolidays(CityHolidays(tt.year)); !maps.Equal(got, tt.want) {
			t.Errorf("CityHolidays(%d) =\n%v\nwant\n%v", tt.year, got, tt.want)
		}
	}
}

func TestEaster(t *testing.T) {
	tests := map[int]string{
		2024: "2024-03-31",
		2025: "2025-04-20",
		2026: "2026-04-05",
		2027: "2027-03-28",
		2028: "2028-04-16",
		2038: "2038-04-25",
	}
	for year, want := range tests {
		if got := easter(year).Format(DateLayout); got != want {
			t.Errorf("easter(%d) = %s, want %s", year, got, want)
		}
	}
}

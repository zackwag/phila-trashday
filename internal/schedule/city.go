package schedule

import "time"

// CityHolidays returns the City of Philadelphia's holidays in year, named
// the way the city names them.
//
// They're the holidays the city lists for trash collection at
// api.phila.gov/phila/trashday/v1: the federal holidays plus Good Friday and
// the day after Thanksgiving. They're computed rather than fetched, because
// that endpoint blocks anything that isn't a browser, and every one of them
// follows a fixed rule.
//
// A holiday on a Sunday is observed the next day, as phila.gov's holidays
// page says. One on a Saturday stays on Saturday, the way the city listed
// Independence Day in 2026, so it doesn't move any pickups.
func CityHolidays(year int) Holidays {
	h := Holidays{}
	fixed := func(month time.Month, day int, name string) {
		d := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
		if d.Weekday() == time.Sunday {
			d = d.AddDate(0, 0, 1)
		}
		h[d] = name
	}

	fixed(time.January, 1, "New Year's Day")
	h[nthWeekday(year, time.January, time.Monday, 3)] = "Martin Luther King, Jr. Day"
	h[nthWeekday(year, time.February, time.Monday, 3)] = "Presidents' Day"
	h[easter(year).AddDate(0, 0, -2)] = "Good Friday"
	h[lastWeekday(year, time.May, time.Monday)] = "Memorial Day"
	fixed(time.June, 19, "Juneteenth")
	fixed(time.July, 4, "Independence Day")
	h[nthWeekday(year, time.September, time.Monday, 1)] = "Labor Day"
	h[nthWeekday(year, time.October, time.Monday, 2)] = "Columbus Day/Indigenous Peoples' Day"
	fixed(time.November, 11, "Veterans Day")
	thanksgiving := nthWeekday(year, time.November, time.Thursday, 4)
	h[thanksgiving] = "Thanksgiving"
	h[thanksgiving.AddDate(0, 0, 1)] = "Thanksgiving Friday"
	fixed(time.December, 25, "Christmas Day")
	return h
}

// nthWeekday returns the nth wd of month, like the third Monday of
// January.
func nthWeekday(year int, month time.Month, wd time.Weekday, n int) time.Time {
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	offset := (int(wd) - int(first.Weekday()) + 7) % 7
	return first.AddDate(0, 0, offset+7*(n-1))
}

// lastWeekday returns the last wd of month, like the last Monday of May.
func lastWeekday(year int, month time.Month, wd time.Weekday) time.Time {
	// Day 0 of the next month is the last day of this one.
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC)
	offset := (int(last.Weekday()) - int(wd) + 7) % 7
	return last.AddDate(0, 0, -offset)
}

// easter returns Western Easter Sunday in year, by the anonymous Gregorian
// algorithm (Meeus, Jones and Butcher).
func easter(year int) time.Time {
	a := year % 19
	b, c := year/100, year%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

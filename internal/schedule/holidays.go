package schedule

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// Holidays maps each day the city observes as a holiday, as returned by
// [Day], to its name.
type Holidays map[time.Time]string

// ParseHolidays reads a holidays file: one holiday per line, as a
// YYYY-MM-DD date optionally followed by its name. Blank lines and anything
// after a # are ignored.
func ParseHolidays(r io.Reader) (Holidays, error) {
	holidays := Holidays{}
	scanner := bufio.NewScanner(r)
	for n := 1; scanner.Scan(); n++ {
		line, _, _ := strings.Cut(scanner.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		d, err := time.Parse(DateLayout, fields[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: %q isn't a YYYY-MM-DD date", n, fields[0])
		}
		holidays[d] = strings.Join(fields[1:], " ")
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read holidays: %w", err)
	}
	return holidays, nil
}

// observed reports whether d is a holiday that affects collections.
func (h Holidays) observed(d time.Time) bool {
	_, ok := h[d]
	return ok && !isWeekend(d)
}

// weekdaysIn returns the holidays from Monday to Friday of the week starting
// on the Monday week, in order.
func (h Holidays) weekdaysIn(week time.Time) []time.Time {
	var days []time.Time
	for i := range 5 {
		if d := week.AddDate(0, 0, i); h.observed(d) {
			days = append(days, d)
		}
	}
	return days
}

// shift moves a primary pickup on d a day later for each of the week's
// observed holidays on or before d, then past any holiday it lands on.
func (h Holidays) shift(d time.Time, observed []time.Time) time.Time {
	moved := d
	for _, holiday := range observed {
		if !holiday.After(d) {
			moved = moved.AddDate(0, 0, 1)
		}
	}
	for h.observed(moved) {
		moved = moved.AddDate(0, 0, 1)
	}
	return moved
}

// Holiday is a day the city observes as a holiday.
type Holiday struct {
	Date time.Time
	Name string
}

// Between returns the holidays from from to to, inclusive, in date order.
// Weekend holidays are left out, since they don't move pickups.
func (h Holidays) Between(from, to time.Time) []Holiday {
	var out []Holiday
	for d, name := range h {
		if !h.observed(d) || d.Before(from) || d.After(to) {
			continue
		}
		out = append(out, Holiday{Date: d, Name: name})
	}
	slices.SortFunc(out, func(a, b Holiday) int { return a.Date.Compare(b.Date) })
	return out
}

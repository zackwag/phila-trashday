// Package schedule works out an address's trash and recycling collection
// dates from its weekly pickup days and the city's holidays.
//
// The holiday rules are the ones phila.gov publishes, as implemented by the
// phila_gov source in Waste Collection Schedule
// (https://github.com/mampfes/hacs_waste_collection_schedule):
//
//   - Holidays on a weekend don't affect collections.
//   - A holiday on a weekday cancels that week's secondary pickup.
//   - The primary pickup moves a day later for each holiday in its Monday to
//     Sunday week that falls on or before its usual day, and on again past
//     any holiday it lands on.
//
// The last rule differs from Waste Collection Schedule in one case. When the
// moved pickup lands on a holiday, Waste Collection Schedule recounts every
// holiday up to that day, so a Monday pickup in a week with holidays on
// Monday and Tuesday moves to Thursday, onto Tuesday's moved pickup. Here it
// moves to Wednesday.
package schedule

import (
	"slices"
	"time"
)

// DateLayout is the format of dates in IDs, JSON and the holidays file.
const DateLayout = "2006-01-02"

// Kind is a type of collection.
type Kind string

// Collection kinds.
const (
	Trash     Kind = "trash"
	Recycling Kind = "recycling"
)

// Slot says which of an address's weekly pickups a collection is.
type Slot string

// Pickup slots.
const (
	// Primary is the weekly pickup for both trash and recycling.
	Primary Slot = "primary"
	// Secondary is the second, trash-only weekly pickup some addresses have.
	Secondary Slot = "secondary"
)

// Days is an address's weekly pickup days.
type Days struct {
	// Primary is the day trash and recycling are collected.
	Primary time.Weekday
	// Secondary is the day of the trash-only pickup. It's only used when
	// HasSecondary is set, since Sunday is the zero Weekday.
	Secondary    time.Weekday
	HasSecondary bool
}

// Collection is one pickup.
type Collection struct {
	// ID names the pickup by the date it's usually on and its slot. A
	// holiday changes Date but not ID, so a feed's subscribers see a moved
	// pickup rather than a removed one and a new one.
	ID string
	// Date is the day of the pickup, as returned by [Day].
	Date time.Time
	// Scheduled is the day it's on when no holiday moves it.
	Scheduled time.Time
	Slot      Slot
	Kinds     []Kind
}

// Moved reports whether a holiday moved the pickup off its usual day.
func (c Collection) Moved() bool {
	return !c.Date.Equal(c.Scheduled)
}

// Summary describes what's collected, like "Trash and recycling".
func (c Collection) Summary() string {
	hasTrash := slices.Contains(c.Kinds, Trash)
	hasRecycling := slices.Contains(c.Kinds, Recycling)
	switch {
	case hasTrash && hasRecycling:
		return "Trash and recycling"
	case hasRecycling:
		return "Recycling"
	default:
		return "Trash"
	}
}

// Day returns t's calendar date, in t's location, as midnight UTC. Every date
// in this package is in that form, so dates can be compared and used as map
// keys.
func Day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Build returns the collections from from to to, inclusive, in date order.
// from and to are dates as returned by [Day].
func Build(days Days, holidays Holidays, from, to time.Time) []Collection {
	var out []Collection
	add := func(c Collection) {
		if c.Date.Before(from) || c.Date.After(to) {
			return
		}
		c.ID = c.Scheduled.Format(DateLayout) + "-" + string(c.Slot)
		out = append(out, c)
	}

	// A pickup can move a day or two later, so start a week early in case
	// one from before the window moves into it.
	for week := monday(from).AddDate(0, 0, -7); !week.After(to); week = week.AddDate(0, 0, 7) {
		observed := holidays.weekdaysIn(week)

		primary := onDay(week, days.Primary)
		add(Collection{
			Date:      holidays.shift(primary, observed),
			Scheduled: primary,
			Slot:      Primary,
			Kinds:     []Kind{Trash, Recycling},
		})

		if days.HasSecondary && days.Secondary != days.Primary && len(observed) == 0 {
			secondary := onDay(week, days.Secondary)
			add(Collection{
				Date:      secondary,
				Scheduled: secondary,
				Slot:      Secondary,
				Kinds:     []Kind{Trash},
			})
		}
	}

	slices.SortStableFunc(out, func(a, b Collection) int {
		return a.Date.Compare(b.Date)
	})
	return mergeSameDay(out)
}

// mergeSameDay folds a collection into the one before it when both are on
// the same day, so a day never has two events. The rules shouldn't allow it,
// but a primary pickup moved onto a day with another pickup would otherwise
// show up twice.
func mergeSameDay(sorted []Collection) []Collection {
	var out []Collection
	for _, c := range sorted {
		if n := len(out); n > 0 && out[n-1].Date.Equal(c.Date) {
			for _, k := range c.Kinds {
				if !slices.Contains(out[n-1].Kinds, k) {
					out[n-1].Kinds = append(out[n-1].Kinds, k)
				}
			}
			continue
		}
		out = append(out, c)
	}
	return out
}

// monday returns the Monday of d's Monday to Sunday week.
func monday(d time.Time) time.Time {
	return d.AddDate(0, 0, -offset(d.Weekday()))
}

// onDay returns the day in the week starting on the Monday week that falls
// on wd.
func onDay(week time.Time, wd time.Weekday) time.Time {
	return week.AddDate(0, 0, offset(wd))
}

// offset is how many days wd is after Monday.
func offset(wd time.Weekday) int {
	return (int(wd) + 6) % 7
}

func isWeekend(d time.Time) bool {
	return d.Weekday() == time.Saturday || d.Weekday() == time.Sunday
}

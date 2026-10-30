// Package ical writes all-day events as an iCalendar (RFC 5545) feed.
package ical

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// lineLimit is the longest a content line can be, in octets, before it has
// to be folded (RFC 5545 section 3.1).
const lineLimit = 75

// Calendar is a feed of all-day events.
type Calendar struct {
	// ProdID identifies the program that wrote the feed.
	ProdID string
	// Name is the calendar's display name.
	Name string
	// Refresh tells subscribers how often to fetch the feed again. Zero
	// leaves it to them.
	Refresh time.Duration
	Events  []Event
}

// Event is an all-day event.
type Event struct {
	// UID identifies the event across fetches, so it has to stay the same
	// when the event changes.
	UID string
	// Date is the event's day, at midnight UTC.
	Date        time.Time
	Summary     string
	Description string
	// Alarm, if set, alerts this long after the start of the day. Negative
	// values alert before it, so -4h is 8 PM the evening before.
	Alarm *time.Duration
}

// Write writes cal to w. stamp is the DTSTAMP of every event, normally the
// time the feed was generated.
func Write(w io.Writer, cal Calendar, stamp time.Time) error {
	l := &lines{w: w}
	l.put("BEGIN:VCALENDAR")
	l.put("VERSION:2.0")
	l.put("PRODID:" + escape(cal.ProdID))
	l.put("CALSCALE:GREGORIAN")
	l.put("METHOD:PUBLISH")
	if cal.Name != "" {
		l.put("X-WR-CALNAME:" + escape(cal.Name))
	}
	if cal.Refresh > 0 {
		l.put("REFRESH-INTERVAL;VALUE=DURATION:" + duration(cal.Refresh))
		l.put("X-PUBLISHED-TTL:" + duration(cal.Refresh))
	}
	for _, e := range cal.Events {
		l.put("BEGIN:VEVENT")
		l.put("UID:" + escape(e.UID))
		l.put("DTSTAMP:" + stamp.UTC().Format("20060102T150405Z"))
		l.put("DTSTART;VALUE=DATE:" + e.Date.Format("20060102"))
		l.put("DTEND;VALUE=DATE:" + e.Date.AddDate(0, 0, 1).Format("20060102"))
		l.put("SUMMARY:" + escape(e.Summary))
		if e.Description != "" {
			l.put("DESCRIPTION:" + escape(e.Description))
		}
		l.put("TRANSP:TRANSPARENT")
		if e.Alarm != nil {
			l.put("BEGIN:VALARM")
			l.put("ACTION:DISPLAY")
			l.put("DESCRIPTION:" + escape(e.Summary))
			l.put("TRIGGER:" + duration(*e.Alarm))
			l.put("END:VALARM")
		}
		l.put("END:VEVENT")
	}
	l.put("END:VCALENDAR")
	return l.err
}

// lines writes content lines, folding long ones and keeping the first
// error.
type lines struct {
	w   io.Writer
	err error
}

func (l *lines) put(s string) {
	if l.err != nil {
		return
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		// Ranging over a string turns invalid bytes into utf8.RuneError,
		// which WriteRune writes as three bytes, so size is never -1.
		size := utf8.RuneLen(r)
		// Fold before a character that would pass the limit, so a
		// multi-byte character is never split. The continuation line's
		// leading space counts toward its length.
		if n+size > lineLimit {
			b.WriteString("\r\n ")
			n = 1
		}
		b.WriteRune(r)
		n += size
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(l.w, b.String()); err != nil {
		l.err = fmt.Errorf("write calendar: %w", err)
	}
}

// escape escapes a TEXT value (RFC 5545 section 3.3.11).
func escape(s string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		";", `\;`,
		",", `\,`,
		"\r\n", `\n`,
		"\n", `\n`,
	).Replace(s)
}

// duration formats d as an RFC 5545 duration, like -PT4H or PT1H30M.
func duration(d time.Duration) string {
	sign := ""
	if d < 0 {
		sign = "-"
		d = -d
	}
	h := int64(d / time.Hour)
	m := int64(d % time.Hour / time.Minute)
	s := int64(d % time.Minute / time.Second)

	var b strings.Builder
	b.WriteString(sign + "PT")
	if h > 0 {
		fmt.Fprintf(&b, "%dH", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dM", m)
	}
	if s > 0 || (h == 0 && m == 0) {
		fmt.Fprintf(&b, "%dS", s)
	}
	return b.String()
}

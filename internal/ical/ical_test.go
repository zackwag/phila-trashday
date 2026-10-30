package ical

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWrite(t *testing.T) {
	alarm := -4 * time.Hour
	cal := Calendar{
		ProdID:  "-//zackwag//phila-trashday//EN",
		Name:    "Trash and recycling",
		Refresh: 6 * time.Hour,
		Events: []Event{
			{
				UID:         "2026-11-11-primary@phila-trashday",
				Date:        time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC),
				Summary:     "Trash and recycling",
				Description: "Moved from Wednesday, November 11 for a holiday.",
				Alarm:       &alarm,
			},
			{
				UID:     "2026-11-14-secondary@phila-trashday",
				Date:    time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC),
				Summary: "Trash",
			},
		},
	}
	stamp := time.Date(2026, 10, 30, 11, 5, 0, 0, time.FixedZone("EDT", -4*60*60))

	var b strings.Builder
	if err := Write(&b, cal, stamp); err != nil {
		t.Fatal(err)
	}

	want := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//zackwag//phila-trashday//EN",
		"CALSCALE:GREGORIAN",
		"METHOD:PUBLISH",
		"X-WR-CALNAME:Trash and recycling",
		"REFRESH-INTERVAL;VALUE=DURATION:PT6H",
		"X-PUBLISHED-TTL:PT6H",
		"BEGIN:VEVENT",
		"UID:2026-11-11-primary@phila-trashday",
		"DTSTAMP:20261030T150500Z",
		"DTSTART;VALUE=DATE:20261112",
		"DTEND;VALUE=DATE:20261113",
		"SUMMARY:Trash and recycling",
		`DESCRIPTION:Moved from Wednesday\, November 11 for a holiday.`,
		"TRANSP:TRANSPARENT",
		"BEGIN:VALARM",
		"ACTION:DISPLAY",
		"DESCRIPTION:Trash and recycling",
		"TRIGGER:-PT4H",
		"END:VALARM",
		"END:VEVENT",
		"BEGIN:VEVENT",
		"UID:2026-11-14-secondary@phila-trashday",
		"DTSTAMP:20261030T150500Z",
		"DTSTART;VALUE=DATE:20261114",
		"DTEND;VALUE=DATE:20261115",
		"SUMMARY:Trash",
		"TRANSP:TRANSPARENT",
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")
	if got := b.String(); got != want {
		t.Errorf("Write() =\n%s\nwant\n%s", got, want)
	}
}

func TestWriteOptionalFields(t *testing.T) {
	var b strings.Builder
	if err := Write(&b, Calendar{ProdID: "x"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, prop := range []string{"X-WR-CALNAME", "REFRESH-INTERVAL", "X-PUBLISHED-TTL", "VEVENT"} {
		if strings.Contains(b.String(), prop) {
			t.Errorf("output has %s, want it left out:\n%s", prop, b.String())
		}
	}
}

func TestFolding(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{"ASCII", "DESCRIPTION:" + strings.Repeat("a", 200)},
		{"multi-byte", "DESCRIPTION:" + strings.Repeat("é", 100)},
		{"invalid UTF-8", "DESCRIPTION:" + strings.Repeat("\xff", 100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			l := &lines{w: &b}
			l.put(tt.line)

			out := b.String()
			if !strings.HasSuffix(out, "\r\n") {
				t.Fatalf("output doesn't end in CRLF: %q", out)
			}
			physical := strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n")
			if len(physical) < 2 {
				t.Fatalf("got %d lines, want the line folded", len(physical))
			}
			for i, p := range physical {
				if len(p) > lineLimit {
					t.Errorf("line %d is %d octets, want at most %d", i, len(p), lineLimit)
				}
				if i > 0 && !strings.HasPrefix(p, " ") {
					t.Errorf("continuation line %d doesn't start with a space: %q", i, p)
				}
			}
			unfolded := strings.ReplaceAll(strings.TrimSuffix(out, "\r\n"), "\r\n ", "")
			if tt.name != "invalid UTF-8" && unfolded != tt.line {
				t.Errorf("unfolded line = %q, want %q", unfolded, tt.line)
			}
		})
	}
}

func TestEscape(t *testing.T) {
	got := escape("a\\b;c,d\ne\r\nf")
	if want := `a\\b\;c\,d\ne\nf`; got != want {
		t.Errorf("escape() = %q, want %q", got, want)
	}
}

func TestDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-4 * time.Hour, "-PT4H"},
		{-24 * time.Hour, "-PT24H"},
		{90 * time.Minute, "PT1H30M"},
		{-(3*time.Hour + 15*time.Minute + 5*time.Second), "-PT3H15M5S"},
		{45 * time.Second, "PT45S"},
		{0, "PT0S"},
	}
	for _, tt := range tests {
		if got := duration(tt.d); got != tt.want {
			t.Errorf("duration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("disk full")
}

func TestWriteError(t *testing.T) {
	err := Write(failingWriter{}, Calendar{ProdID: "x"}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v, want the writer's error", err)
	}
}

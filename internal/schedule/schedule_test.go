package schedule

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func date(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(DateLayout, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func holidays(t *testing.T, dates ...string) Holidays {
	t.Helper()
	h := Holidays{}
	for _, s := range dates {
		h[date(t, s)] = "holiday"
	}
	return h
}

// format renders collections as "date id summary" lines, which keeps the
// expected values in the table short and readable.
func format(cs []Collection) []string {
	lines := make([]string, len(cs))
	for i, c := range cs {
		lines[i] = c.Date.Format(DateLayout) + " " + c.ID + " " + c.Summary()
	}
	return lines
}

func TestBuild(t *testing.T) {
	wedSat := Days{Primary: time.Wednesday, Secondary: time.Saturday, HasSecondary: true}

	tests := []struct {
		name     string
		days     Days
		holidays []string
		from, to string
		want     []string
	}{
		{
			name: "week without holidays",
			days: wedSat,
			from: "2026-11-02",
			to:   "2026-11-08",
			want: []string{
				"2026-11-04 2026-11-04-primary Trash and recycling",
				"2026-11-07 2026-11-07-secondary Trash",
			},
		},
		{
			name:     "holiday on the primary day moves it and cancels the secondary",
			days:     wedSat,
			holidays: []string{"2026-11-11"},
			from:     "2026-11-09",
			to:       "2026-11-15",
			want: []string{
				"2026-11-12 2026-11-11-primary Trash and recycling",
			},
		},
		{
			name:     "holiday after the primary day only cancels the secondary",
			days:     wedSat,
			holidays: []string{"2026-11-26"},
			from:     "2026-11-23",
			to:       "2026-11-29",
			want: []string{
				"2026-11-25 2026-11-25-primary Trash and recycling",
			},
		},
		{
			name:     "holiday before the primary day moves it",
			days:     Days{Primary: time.Thursday},
			holidays: []string{"2026-01-19"},
			from:     "2026-01-19",
			to:       "2026-01-25",
			want: []string{
				"2026-01-23 2026-01-22-primary Trash and recycling",
			},
		},
		{
			name:     "Friday holiday moves a Friday pickup to Saturday",
			days:     Days{Primary: time.Friday},
			holidays: []string{"2026-12-25"},
			from:     "2026-12-21",
			to:       "2026-12-27",
			want: []string{
				"2026-12-26 2026-12-25-primary Trash and recycling",
			},
		},
		{
			name:     "landing on a second holiday moves it again",
			days:     Days{Primary: time.Thursday},
			holidays: []string{"2026-11-26", "2026-11-27"},
			from:     "2026-11-23",
			to:       "2026-11-29",
			want: []string{
				"2026-11-28 2026-11-26-primary Trash and recycling",
			},
		},
		{
			name:     "back-to-back holidays move an early pickup past both, not two days further",
			days:     Days{Primary: time.Monday},
			holidays: []string{"2026-01-19", "2026-01-20"},
			from:     "2026-01-19",
			to:       "2026-01-25",
			want: []string{
				"2026-01-21 2026-01-19-primary Trash and recycling",
			},
		},
		{
			name:     "two holidays before the pickup move it two days",
			days:     Days{Primary: time.Wednesday},
			holidays: []string{"2026-01-19", "2026-01-20"},
			from:     "2026-01-19",
			to:       "2026-01-25",
			want: []string{
				"2026-01-23 2026-01-21-primary Trash and recycling",
			},
		},
		{
			name:     "weekend holidays are ignored",
			days:     wedSat,
			holidays: []string{"2026-07-04", "2026-07-05"},
			from:     "2026-06-29",
			to:       "2026-07-05",
			want: []string{
				"2026-07-01 2026-07-01-primary Trash and recycling",
				"2026-07-04 2026-07-04-secondary Trash",
			},
		},
		{
			name:     "a pickup moved into the window is included",
			days:     wedSat,
			holidays: []string{"2026-11-11"},
			from:     "2026-11-12",
			to:       "2026-11-12",
			want: []string{
				"2026-11-12 2026-11-11-primary Trash and recycling",
			},
		},
		{
			name:     "a pickup moved out of the window is left out",
			days:     wedSat,
			holidays: []string{"2026-11-11"},
			from:     "2026-11-09",
			to:       "2026-11-11",
			want:     nil,
		},
		{
			name: "a secondary day that's the primary day is ignored",
			days: Days{Primary: time.Wednesday, Secondary: time.Wednesday, HasSecondary: true},
			from: "2026-11-02",
			to:   "2026-11-08",
			want: []string{
				"2026-11-04 2026-11-04-primary Trash and recycling",
			},
		},
		{
			name: "Sunday secondary isn't the zero value by accident",
			days: Days{Primary: time.Wednesday},
			from: "2026-11-02",
			to:   "2026-11-08",
			want: []string{
				"2026-11-04 2026-11-04-primary Trash and recycling",
			},
		},
		{
			name: "several weeks in order",
			days: wedSat,
			// Matches what Waste Collection Schedule shows for a Wednesday
			// and Saturday address over Veterans Day and Thanksgiving.
			holidays: []string{"2026-11-11", "2026-11-26"},
			from:     "2026-10-31",
			to:       "2026-11-30",
			want: []string{
				"2026-10-31 2026-10-31-secondary Trash",
				"2026-11-04 2026-11-04-primary Trash and recycling",
				"2026-11-07 2026-11-07-secondary Trash",
				"2026-11-12 2026-11-11-primary Trash and recycling",
				"2026-11-18 2026-11-18-primary Trash and recycling",
				"2026-11-21 2026-11-21-secondary Trash",
				"2026-11-25 2026-11-25-primary Trash and recycling",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := format(Build(tt.days, holidays(t, tt.holidays...), date(t, tt.from), date(t, tt.to)))
			if !slices.Equal(got, tt.want) {
				t.Errorf("Build() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

func TestBuildMovedKeepsID(t *testing.T) {
	days := Days{Primary: time.Wednesday}
	from, to := date(t, "2026-11-09"), date(t, "2026-11-15")

	before := Build(days, nil, from, to)
	after := Build(days, holidays(t, "2026-11-11"), from, to)

	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("got %d and %d collections, want 1 each", len(before), len(after))
	}
	if before[0].ID != after[0].ID {
		t.Errorf("ID changed from %q to %q when a holiday moved the pickup", before[0].ID, after[0].ID)
	}
	if before[0].Moved() {
		t.Error("Moved() = true without a holiday")
	}
	if !after[0].Moved() {
		t.Error("Moved() = false after a holiday moved the pickup")
	}
}

func TestMergeSameDay(t *testing.T) {
	d := date(t, "2026-11-12")
	got := mergeSameDay([]Collection{
		{ID: "a", Date: d, Kinds: []Kind{Trash, Recycling}},
		{ID: "b", Date: d, Kinds: []Kind{Trash}},
		{ID: "c", Date: d.AddDate(0, 0, 1), Kinds: []Kind{Trash}},
	})
	if len(got) != 2 {
		t.Fatalf("got %d collections, want 2", len(got))
	}
	if got[0].ID != "a" || !slices.Equal(got[0].Kinds, []Kind{Trash, Recycling}) {
		t.Errorf("first = %+v, want a with trash and recycling", got[0])
	}

	got = mergeSameDay([]Collection{
		{ID: "a", Date: d, Kinds: []Kind{Trash}},
		{ID: "b", Date: d, Kinds: []Kind{Recycling}},
	})
	if len(got) != 1 || !slices.Equal(got[0].Kinds, []Kind{Trash, Recycling}) {
		t.Errorf("got %+v, want one collection with trash and recycling", got)
	}
}

func TestSummary(t *testing.T) {
	tests := []struct {
		kinds []Kind
		want  string
	}{
		{[]Kind{Trash, Recycling}, "Trash and recycling"},
		{[]Kind{Recycling, Trash}, "Trash and recycling"},
		{[]Kind{Trash}, "Trash"},
		{[]Kind{Recycling}, "Recycling"},
	}
	for _, tt := range tests {
		if got := (Collection{Kinds: tt.kinds}).Summary(); got != tt.want {
			t.Errorf("Summary(%v) = %q, want %q", tt.kinds, got, tt.want)
		}
	}
}

func TestDay(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 11 PM in Philadelphia is already the next day in UTC.
	got := Day(time.Date(2026, 11, 3, 23, 0, 0, 0, ny))
	if want := date(t, "2026-11-03"); !got.Equal(want) {
		t.Errorf("Day() = %v, want %v", got, want)
	}
}

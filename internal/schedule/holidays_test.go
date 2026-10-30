package schedule

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

func TestParseHolidays(t *testing.T) {
	input := `# City holidays that affect collection

2026-11-11 Veterans Day
2026-11-26   Thanksgiving Day   # comment after the name
2026-12-25
2027-01-01 New Year's Day
`
	got, err := ParseHolidays(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"2026-11-11": "Veterans Day",
		"2026-11-26": "Thanksgiving Day",
		"2026-12-25": "",
		"2027-01-01": "New Year's Day",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d holidays, want %d: %v", len(got), len(want), got)
	}
	for s, name := range want {
		if g, ok := got[date(t, s)]; !ok || g != name {
			t.Errorf("holiday %s = %q (present %v), want %q", s, g, ok, name)
		}
	}

	if years := got.Years(); !slices.Equal(years, []int{2026, 2027}) {
		t.Errorf("Years() = %v, want [2026 2027]", years)
	}
}

func TestParseHolidaysErrors(t *testing.T) {
	_, err := ParseHolidays(strings.NewReader("2026-11-11\n11/26/2026 Thanksgiving\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("err = %v, want one naming line 2", err)
	}

	readErr := errors.New("disk on fire")
	_, err = ParseHolidays(iotest.ErrReader(readErr))
	if !errors.Is(err, readErr) {
		t.Errorf("err = %v, want it to wrap %v", err, readErr)
	}
}

func TestParseHolidaysEmpty(t *testing.T) {
	got, err := ParseHolidays(strings.NewReader("# nothing yet\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || len(got.Years()) != 0 {
		t.Errorf("got %v, want no holidays", got)
	}
}

func TestBetween(t *testing.T) {
	h := Holidays{
		date(t, "2026-07-04"): "Independence Day", // a Saturday
		date(t, "2026-11-26"): "Thanksgiving Day",
		date(t, "2026-11-11"): "Veterans Day",
		date(t, "2026-12-25"): "Christmas Day",
		date(t, "2027-01-01"): "New Year's Day",
	}
	got := h.Between(date(t, "2026-07-01"), date(t, "2026-12-25"))
	var names []string
	for _, holiday := range got {
		names = append(names, holiday.Date.Format(DateLayout)+" "+holiday.Name)
	}
	want := []string{"2026-11-11 Veterans Day", "2026-11-26 Thanksgiving Day", "2026-12-25 Christmas Day"}
	if !slices.Equal(names, want) {
		t.Errorf("Between() = %v, want %v", names, want)
	}
}

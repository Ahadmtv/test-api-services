package scheduling

import (
	"errors"
	"testing"
	"time"
)

func TestValidateWorkingHoursAllowsMultipleAndAdjacentIntervals(t *testing.T) {
	intervals := []WorkingInterval{
		{Weekday: 1, OpensAt: "09:00", ClosesAt: "12:00"},
		{Weekday: 1, OpensAt: "12:00", ClosesAt: "13:00"},
		{Weekday: 1, OpensAt: "14:00", ClosesAt: "17:00"},
	}
	if err := ValidateWorkingHours(intervals); err != nil {
		t.Fatalf("valid multiple intervals rejected: %v", err)
	}
}

func TestValidateTimeOff(t *testing.T) {
	start := time.Date(2030, time.March, 20, 9, 0, 0, 0, time.FixedZone("Tehran", 3*60*60+30*60))
	end := start.Add(time.Hour)
	if err := ValidateTimeOff("blocked", start, end, ""); err != nil {
		t.Fatalf("valid time-off rejected: %v", err)
	}
	for _, tc := range []struct {
		kind, reason string
		start, end   time.Time
	}{
		{"unknown", "", start, end},
		{"time_off", "", start, start},
		{"time_off", "", end, start},
		{"blocked", string(make([]rune, 501)), start, end},
	} {
		if err := ValidateTimeOff(tc.kind, tc.start, tc.end, tc.reason); !errors.Is(err, ErrInvalidTimeOff) {
			t.Errorf("ValidateTimeOff(%q) = %v, want ErrInvalidTimeOff", tc.kind, err)
		}
	}
}

func TestValidateWorkingHoursRejectsInvalidIntervals(t *testing.T) {
	cases := [][]WorkingInterval{
		{{Weekday: 0, OpensAt: "09:00", ClosesAt: "10:00"}},
		{{Weekday: 8, OpensAt: "09:00", ClosesAt: "10:00"}},
		{{Weekday: 1, OpensAt: "22:00", ClosesAt: "02:00"}},
		{{Weekday: 1, OpensAt: "09:00", ClosesAt: "09:00"}},
		{{Weekday: 1, OpensAt: "not-time", ClosesAt: "10:00"}},
		{
			{Weekday: 2, OpensAt: "09:00", ClosesAt: "12:00"},
			{Weekday: 2, OpensAt: "11:30", ClosesAt: "13:00"},
		},
	}
	for i, intervals := range cases {
		if err := ValidateWorkingHours(intervals); !errors.Is(err, ErrInvalidSchedule) {
			t.Errorf("case %d: got %v, want ErrInvalidSchedule", i, err)
		}
	}
}

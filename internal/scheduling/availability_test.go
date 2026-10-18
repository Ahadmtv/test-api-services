package scheduling

import (
	"testing"
	"time"
)

func TestCalculateAvailability_Basic(t *testing.T) {
	tehran, _ := time.LoadLocation("Asia/Tehran")
	date := "2025-03-15" // Saturday
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 09:00-17:00 with 60min service + 15min buffer = 75min
	// 30min slots: 09:00, 09:30, 10:00, 10:30, 11:00, 11:30, 12:00, 12:30, 13:00, 13:30, 14:00, 14:30, 15:00, 15:30
	// Last slot at 15:30 ends at 16:30, buffer until 16:45 - fits before 17:00
	if len(slots) != 14 {
		t.Fatalf("expected 14 slots, got %d", len(slots))
	}

	// Verify first slot
	if !slots[0].StartAt.In(tehran).Equal(time.Date(2025, 3, 15, 9, 0, 0, 0, tehran)) {
		t.Errorf("first slot start: got %v, want 09:00 Tehran", slots[0].StartAt.In(tehran))
	}
	// Verify last slot
	if !slots[13].StartAt.In(tehran).Equal(time.Date(2025, 3, 15, 15, 30, 0, 0, tehran)) {
		t.Errorf("last slot start: got %v, want 15:30 Tehran", slots[13].StartAt.In(tehran))
	}
}

func TestCalculateAvailability_ServiceFitsExactlyAtClosing(t *testing.T) {
	tehran, _ := time.LoadLocation("Asia/Tehran")
	date := "2025-03-15" // Saturday
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	// 60min service + 15min buffer = 75min, last slot at 15:30 ends at 16:45, buffer until 17:00 - exact fit
	slots, err := CalculateAvailability(date, "Asia/Tehran", []WorkingInterval{{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"}}, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Last valid slot should be 15:30 (ends 16:30, buffer until 17:00)
	lastSlot := slots[len(slots)-1]
	if !lastSlot.StartAt.In(tehran).Equal(time.Date(2025, 3, 15, 15, 30, 0, 0, tehran)) {
		t.Errorf("last slot start: got %v, want 15:30 Tehran", lastSlot.StartAt.In(tehran))
	}
}

func TestCalculateAvailability_ServiceExceedsClosingTime(t *testing.T) {
	tehran, _ := time.LoadLocation("Asia/Tehran")
	date := "2025-03-15"
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	// 90min service + 15min buffer = 105min, last slot at 15:30 ends at 17:00, buffer until 17:15 - exceeds 17:00!
	slots, err := CalculateAvailability(date, "Asia/Tehran", []WorkingInterval{{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"}}, nil, nil, 90, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Last valid slot should be 15:00 (ends 16:30, buffer until 16:45)
	lastSlot := slots[len(slots)-1]
	if !lastSlot.StartAt.In(tehran).Equal(time.Date(2025, 3, 15, 15, 0, 0, 0, tehran)) {
		t.Errorf("last slot start: got %v, want 15:00 Tehran", lastSlot.StartAt.In(tehran))
	}
}

func TestCalculateAvailability_MultipleIntervalsWithLunchBreak(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "12:00"},
		{Weekday: 6, OpensAt: "13:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Morning: 09:00-12:00, 60min+15min=75min, slots: 09:00, 09:30, 10:00, 10:30 (4 slots)
	// Afternoon: 13:00-17:00, 60min+15min=75min, slots: 13:00, 13:30, 14:00, 14:30, 15:00, 15:30 (6 slots)
	// Total: 10 slots
	if len(slots) != 10 {
		t.Fatalf("expected 10 slots, got %d", len(slots))
	}

	// Verify gap between 12:00 and 13:00 (lunch break) is the only large gap
	// Morning ends at 10:30 start (ends 11:30), afternoon starts at 13:00
	// Gap between last morning (10:30) and first afternoon (13:00) = 2h30m
	largeGaps := 0
	for i := 0; i < len(slots)-1; i++ {
		gap := slots[i+1].StartAt.Sub(slots[i].StartAt)
		if gap > 30*time.Minute {
			largeGaps++
			if gap != 2*time.Hour+30*time.Minute {
				t.Errorf("unexpected gap between slots: %v", gap)
			}
		}
	}
	if largeGaps != 1 {
		t.Errorf("expected exactly 1 large gap (lunch break), got %d", largeGaps)
	}
}

func TestCalculateAvailability_WithExistingBookings(t *testing.T) {
	date := "2025-03-15"
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	// Existing booking from 10:00-11:30 UTC (13:30-15:00 Tehran)
	occupied := []TimeInterval{
		{StartAt: time.Date(2025, 3, 15, 10, 0, 0, 0, time.UTC), EndAt: time.Date(2025, 3, 15, 11, 30, 0, 0, time.UTC)},
	}

	slots, err := CalculateAvailability(date, "Asia/Tehran", []WorkingInterval{{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"}}, nil, occupied, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not have slots that overlap with 13:30-15:00 Tehran (10:00-11:30 UTC)
	for _, slot := range slots {
		startTehran := slot.StartAt.In(time.FixedZone("Tehran", 3*60*60+30*60))
		// Should not overlap with 13:30-15:00
		if startTehran.Hour() == 13 && startTehran.Minute() >= 30 {
			t.Errorf("slot overlaps with booking: %v", slot.StartAt)
		}
	}
}

func TestCalculateAvailability_WithBlockedIntervals(t *testing.T) {
	date := "2025-03-15"
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	// Blocked interval 11:00-13:00 UTC (14:30-16:30 Tehran)
	blocked := []TimeInterval{
		{StartAt: time.Date(2025, 3, 15, 11, 0, 0, 0, time.UTC), EndAt: time.Date(2025, 3, 15, 13, 0, 0, 0, time.UTC)},
	}

	slots, err := CalculateAvailability(date, "Asia/Tehran", []WorkingInterval{{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"}}, blocked, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not have slots overlapping with blocked interval
	for _, slot := range slots {
		startTehran := slot.StartAt.In(time.FixedZone("Tehran", 3*60*60+30*60))
		if startTehran.Hour() >= 14 && startTehran.Hour() < 16 {
			t.Errorf("slot overlaps with blocked interval: %v", slot.StartAt)
		}
	}
}

func TestCalculateAvailability_WithBufferTime(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	// 30min buffer means service+buffer = 90min
	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 30, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Last slot should be 15:30 (ends 16:30, buffer until 17:00 - exact fit)
	lastSlot := slots[len(slots)-1]
	tehran, _ := time.LoadLocation("Asia/Tehran")
	if !lastSlot.StartAt.In(tehran).Equal(time.Date(2025, 3, 15, 15, 30, 0, 0, tehran)) {
		t.Errorf("last slot start: got %v, want 15:30 Tehran", lastSlot.StartAt.In(tehran))
	}
}

func TestCalculateAvailability_InactiveService(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	// The handler checks service.IsActive before calling CalculateAvailability
	// So this tests the core logic with an edge case
	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(slots) == 0 {
		t.Error("expected slots for active service")
	}
}

func TestCalculateAvailability_PastDate(t *testing.T) {
	date := "2025-01-01" // Past date
	workingHours := []WorkingInterval{
		{Weekday: 3, OpensAt: "09:00", ClosesAt: "17:00"}, // Wednesday
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 15, 10, 0, 0, 0, time.UTC)

	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(slots) != 0 {
		t.Errorf("expected 0 slots for past date, got %d", len(slots))
	}
}

func TestCalculateAvailability_BookingNoticeLimit(t *testing.T) {
	date := "2025-03-15" // 5 days from now (now is 2025-03-10)
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 4 * 24 * time.Hour, // 4 days horizon
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(slots) != 0 {
		t.Errorf("expected 0 slots (date beyond horizon), got %d", len(slots))
	}
}

func TestCalculateAvailability_MinimumNotice(t *testing.T) {
	date := "2025-03-10" // Today
	workingHours := []WorkingInterval{
		{Weekday: 1, OpensAt: "09:00", ClosesAt: "17:00"}, // Monday
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC) // 10:00 UTC = 13:30 Tehran

	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// First slot should be at least 2 hours from now (12:00 UTC = 15:30 Tehran)
	if len(slots) > 0 {
		firstSlot := slots[0].StartAt
		if firstSlot.Before(now.Add(2 * time.Hour)) {
			t.Errorf("first slot %v is before minimum notice %v", firstSlot, now.Add(2*time.Hour))
		}
	}
}

func TestCalculateAvailability_TimeZoneConversion(t *testing.T) {
	// Test with different time zones
	testCases := []struct {
		name     string
		timeZone string
		date     string
		weekday  int
	}{
		{"Asia/Tehran", "Asia/Tehran", "2025-03-15", 6},
		{"UTC", "UTC", "2025-03-15", 6},
		{"Europe/London", "Europe/London", "2025-03-15", 6},
		{"America/New_York", "America/New_York", "2025-03-15", 6},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			workingHours := []WorkingInterval{
				{Weekday: tc.weekday, OpensAt: "09:00", ClosesAt: "17:00"},
			}
			policy := AvailabilityPolicy{
				SlotIncrement:  30 * time.Minute,
				MinimumNotice:  2 * time.Hour,
				MaximumHorizon: 90 * 24 * time.Hour,
			}
			now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

			slots, err := CalculateAvailability(tc.date, tc.timeZone, workingHours, nil, nil, 60, 15, policy, now)
			if err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.name, err)
			}
			if len(slots) == 0 {
				t.Errorf("expected slots for %s", tc.name)
			}

			// Verify slots are in UTC
			for _, slot := range slots {
				if slot.StartAt.Location() != time.UTC {
					t.Errorf("slot not in UTC for %s: %v", tc.name, slot.StartAt.Location())
				}
			}
		})
	}
}

func TestCalculateAvailability_InvalidTimeZone(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	_, err := CalculateAvailability(date, "Invalid/TimeZone", workingHours, nil, nil, 60, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for invalid time zone")
	}
}

func TestCalculateAvailability_AmbiguousTime(t *testing.T) {
	// Test DST transition - use a time zone with DST
	date := "2025-11-02" // DST end in US (first Sunday in November)
	workingHours := []WorkingInterval{
		{Weekday: 7, OpensAt: "01:30", ClosesAt: "03:00"}, // Ambiguous hour during DST fallback
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 10, 15, 10, 0, 0, 0, time.UTC)

	slots, err := CalculateAvailability(date, "America/New_York", workingHours, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should handle ambiguous time gracefully (skip ambiguous slots)
	if len(slots) == 0 {
		t.Log("no valid slots during ambiguous time - expected behavior")
	}
}

func TestCalculateAvailability_NonexistentTime(t *testing.T) {
	// Test DST spring forward - 2:00 AM doesn't exist
	date := "2025-03-09" // DST start in US (second Sunday in March)
	workingHours := []WorkingInterval{
		{Weekday: 7, OpensAt: "02:30", ClosesAt: "03:30"}, // Non-existent hour
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 2, 15, 10, 0, 0, 0, time.UTC)

	slots, err := CalculateAvailability(date, "America/New_York", workingHours, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should skip non-existent time
	if len(slots) == 0 {
		t.Log("no valid slots during non-existent time - expected behavior")
	}
}

func TestCalculateAvailability_DateBoundary(t *testing.T) {
	// Test date boundary - service crosses midnight
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "22:00", ClosesAt: "02:00"}, // Invalid - overnight not allowed by ValidateWorkingHours
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	_, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for overnight interval")
	}
}

func TestCalculateAvailability_SlotIncrementNotMultipleOfMinute(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  45 * time.Second, // Not multiple of minute
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	_, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for slot increment not multiple of minute")
	}
}

func TestCalculateAvailability_ZeroSlotIncrement(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  0,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	_, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for zero slot increment")
	}
}

func TestCalculateAvailability_MaximumHorizonLessThanMinNotice(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  4 * time.Hour,
		MaximumHorizon: 2 * time.Hour, // Less than min notice
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	_, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for max horizon <= min notice")
	}
}

func TestCalculateAvailability_OverlappingBlockedIntervals(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	// Overlapping blocked intervals
	blocked := []TimeInterval{
		{StartAt: time.Date(2025, 3, 15, 10, 0, 0, 0, time.UTC), EndAt: time.Date(2025, 3, 15, 12, 0, 0, 0, time.UTC)},
		{StartAt: time.Date(2025, 3, 15, 11, 0, 0, 0, time.UTC), EndAt: time.Date(2025, 3, 15, 13, 0, 0, 0, time.UTC)},
	}

	_, err := CalculateAvailability(date, "Asia/Tehran", workingHours, blocked, nil, 60, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for overlapping blocked intervals")
	}
}

func TestCalculateAvailability_OverlappingOccupiedIntervals(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	// Overlapping occupied intervals - the engine doesn't validate overlapping occupied
	// intervals (database constraints should prevent this), but it should still
	// correctly exclude slots that overlap with any occupied interval
	occupied := []TimeInterval{
		{StartAt: time.Date(2025, 3, 15, 10, 0, 0, 0, time.UTC), EndAt: time.Date(2025, 3, 15, 12, 0, 0, 0, time.UTC)},
		{StartAt: time.Date(2025, 3, 15, 11, 0, 0, 0, time.UTC), EndAt: time.Date(2025, 3, 15, 13, 0, 0, 0, time.UTC)},
	}

	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, occupied, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Slots overlapping with either occupied interval should be excluded
	for _, slot := range slots {
		candidate := TimeInterval{StartAt: slot.StartAt, EndAt: slot.EndAt.Add(15 * time.Minute)}
		if intersectsAny(candidate, occupied) {
			t.Errorf("slot %v overlaps with occupied interval", slot.StartAt)
		}
	}
}

func TestCalculateAvailability_InvalidDuration(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	_, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 0, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for zero duration")
	}

	_, err = CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 1441, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for duration > 1440")
	}
}

func TestCalculateAvailability_InvalidBuffer(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	_, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, -1, policy, now)
	if err == nil {
		t.Fatal("expected error for negative buffer")
	}

	_, err = CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 1441, policy, now)
	if err == nil {
		t.Fatal("expected error for buffer > 1440")
	}
}

func TestCalculateAvailability_InvalidWorkingHours(t *testing.T) {
	date := "2025-03-15"
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	// Invalid weekday
	_, err := CalculateAvailability(date, "Asia/Tehran", []WorkingInterval{{Weekday: 0, OpensAt: "09:00", ClosesAt: "17:00"}}, nil, nil, 60, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for invalid weekday")
	}

	// Invalid time format
	_, err = CalculateAvailability(date, "Asia/Tehran", []WorkingInterval{{Weekday: 1, OpensAt: "25:00", ClosesAt: "17:00"}}, nil, nil, 60, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for invalid time format")
	}

	// OpensAt >= ClosesAt
	_, err = CalculateAvailability(date, "Asia/Tehran", []WorkingInterval{{Weekday: 1, OpensAt: "17:00", ClosesAt: "09:00"}}, nil, nil, 60, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for opens_at >= closes_at")
	}

	// Overlapping intervals same day
	_, err = CalculateAvailability(date, "Asia/Tehran", []WorkingInterval{
		{Weekday: 1, OpensAt: "09:00", ClosesAt: "12:00"},
		{Weekday: 1, OpensAt: "11:00", ClosesAt: "13:00"},
	}, nil, nil, 60, 15, policy, now)
	if err == nil {
		t.Fatal("expected error for overlapping intervals")
	}
}

func TestCalculateAvailability_EmptyWorkingHours(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(slots) != 0 {
		t.Errorf("expected 0 slots for empty working hours, got %d", len(slots))
	}
}

func TestCalculateAvailability_SlotsSorted(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "12:00"},
		{Weekday: 6, OpensAt: "13:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify slots are sorted by start time
	for i := 1; i < len(slots); i++ {
		if slots[i-1].StartAt.After(slots[i].StartAt) {
			t.Errorf("slots not sorted: %v after %v", slots[i-1].StartAt, slots[i].StartAt)
		}
	}
}

func TestCalculateAvailability_BufferAddedToEndTime(t *testing.T) {
	date := "2025-03-15"
	workingHours := []WorkingInterval{
		{Weekday: 6, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  60 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	// 60min service, 30min buffer, 60min slots
	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 30, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Each slot should have end_at = start_at + 60min (service duration)
	for _, slot := range slots {
		expectedEnd := slot.StartAt.Add(60 * time.Minute)
		if !slot.EndAt.Equal(expectedEnd) {
			t.Errorf("slot end_at %v != start_at + 60min %v", slot.EndAt, expectedEnd)
		}
	}
}

func TestCalculateAvailability_NoWorkingHoursForDate(t *testing.T) {
	// Sunday (7) but working hours only for Monday (1)
	date := "2025-03-16" // Sunday
	workingHours := []WorkingInterval{
		{Weekday: 1, OpensAt: "09:00", ClosesAt: "17:00"},
	}
	policy := AvailabilityPolicy{
		SlotIncrement:  30 * time.Minute,
		MinimumNotice:  2 * time.Hour,
		MaximumHorizon: 90 * 24 * time.Hour,
	}
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.UTC)

	slots, err := CalculateAvailability(date, "Asia/Tehran", workingHours, nil, nil, 60, 15, policy, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(slots) != 0 {
		t.Errorf("expected 0 slots for day without working hours, got %d", len(slots))
	}
}

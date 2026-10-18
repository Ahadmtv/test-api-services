package scheduling

import (
	"errors"
	"fmt"
	"sort"
	"time"
	_ "time/tzdata"
)

type TimeInterval struct {
	StartAt time.Time
	EndAt   time.Time
}

type AvailableSlot struct {
	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`
}

type AvailabilityPolicy struct {
	SlotIncrement  time.Duration
	MinimumNotice  time.Duration
	MaximumHorizon time.Duration
}

var ErrInvalidAvailability = errors.New("invalid availability configuration")
var ErrInvalidLocalDate = errors.New("invalid provider-local date")
var ErrInvalidTimeZone = errors.New("invalid provider time zone")

type localTimeError struct {
	ambiguous bool
}

func (e localTimeError) Error() string {
	if e.ambiguous {
		return "ambiguous local time"
	}
	return "nonexistent local time"
}

func LocalDayRange(date, timeZone string) (TimeInterval, error) {
	day, location, err := parseLocalDate(date, timeZone)
	if err != nil {
		return TimeInterval{}, err
	}
	return localDayRange(day, location)
}

func localDayRange(day time.Time, location *time.Location) (TimeInterval, error) {
	start, err := resolveWallTime(day, 0, location)
	if err != nil {
		return TimeInterval{}, err
	}
	end, err := resolveWallTime(day.AddDate(0, 0, 1), 0, location)
	if err != nil {
		return TimeInterval{}, err
	}
	return TimeInterval{StartAt: start.UTC(), EndAt: end.UTC()}, nil
}

func CalculateAvailability(date, timeZone string, workingHours []WorkingInterval, blocked, occupied []TimeInterval, durationMinutes, bufferMinutes int, policy AvailabilityPolicy, now time.Time) ([]AvailableSlot, error) {
	day, location, err := parseLocalDate(date, timeZone)
	if err != nil {
		return nil, err
	}
	if err := ValidateWorkingHours(workingHours); err != nil {
		return nil, fmt.Errorf("%w: working hours", ErrInvalidAvailability)
	}
	if durationMinutes < 1 || durationMinutes > 1440 || bufferMinutes < 0 || bufferMinutes > 1440 || policy.SlotIncrement <= 0 || policy.SlotIncrement%time.Minute != 0 || policy.MinimumNotice < 0 || policy.MaximumHorizon <= policy.MinimumNotice {
		return nil, ErrInvalidAvailability
	}
	if err := validateIntervals(blocked); err != nil {
		return nil, fmt.Errorf("%w: time off", ErrInvalidAvailability)
	}
	for i := range blocked {
		for j := i + 1; j < len(blocked); j++ {
			if overlaps(blocked[i], blocked[j]) {
				return nil, fmt.Errorf("%w: overlapping time off", ErrInvalidAvailability)
			}
		}
	}
	if err := validateIntervals(occupied); err != nil {
		return nil, fmt.Errorf("%w: occupied intervals", ErrInvalidAvailability)
	}

	// Filter working hours to only include intervals for the requested date's weekday
	weekday := int(day.Weekday())
	if weekday == 0 {
		weekday = 7 // Sunday = 7 in ISO 8601
	}
	filteredWorkingHours := make([]WorkingInterval, 0)
	for _, interval := range workingHours {
		if interval.Weekday == weekday {
			filteredWorkingHours = append(filteredWorkingHours, interval)
		}
	}

	dayRange, err := localDayRange(day, location)
	if err != nil {
		return nil, fmt.Errorf("%w: local day boundary", ErrInvalidAvailability)
	}
	cutoffStart := now.UTC().Add(policy.MinimumNotice)
	cutoffEnd := now.UTC().Add(policy.MaximumHorizon)
	slotStep := int(policy.SlotIncrement / time.Minute)
	serviceDuration := time.Duration(durationMinutes) * time.Minute
	bufferDuration := time.Duration(bufferMinutes) * time.Minute
	slots := make([]AvailableSlot, 0)

	for _, interval := range filteredWorkingHours {
		openMinute := parseMinuteOfDay(interval.OpensAt)
		closeMinute := parseMinuteOfDay(interval.ClosesAt)
		closeAt, err := resolveWallTime(day, closeMinute, location)
		if err != nil {
			return nil, fmt.Errorf("%w: ambiguous or nonexistent working-hour boundary", ErrInvalidAvailability)
		}
		for minute := openMinute; minute < closeMinute; minute += slotStep {
			startAt, err := resolveWallTime(day, minute, location)
			if _, ok := err.(localTimeError); ok {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("%w: working-hour start", ErrInvalidAvailability)
			}
			endAt := startAt.Add(serviceDuration)
			occupiedUntil := endAt.Add(bufferDuration)
			if startAt.Before(dayRange.StartAt) || !startAt.Before(dayRange.EndAt) || startAt.Before(cutoffStart) || !startAt.Before(cutoffEnd) {
				continue
			}
			if occupiedUntil.After(closeAt) {
				continue
			}
			candidate := TimeInterval{StartAt: startAt, EndAt: occupiedUntil}
			if intersectsAny(candidate, blocked) || intersectsAny(candidate, occupied) {
				continue
			}
			slots = append(slots, AvailableSlot{StartAt: startAt.UTC(), EndAt: endAt.UTC()})
		}
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].StartAt.Before(slots[j].StartAt) })
	return slots, nil
}

func parseLocalDate(value, timeZone string) (time.Time, *time.Location, error) {
	day, err := time.Parse("2006-01-02", value)
	if err != nil || day.Format("2006-01-02") != value {
		return time.Time{}, nil, ErrInvalidLocalDate
	}
	if timeZone == "" || timeZone == "Local" {
		return time.Time{}, nil, ErrInvalidTimeZone
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return time.Time{}, nil, ErrInvalidTimeZone
	}
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC), location, nil
}

func resolveWallTime(day time.Time, minuteOfDay int, location *time.Location) (time.Time, error) {
	wall := time.Date(day.Year(), day.Month(), day.Day(), minuteOfDay/60, minuteOfDay%60, 0, 0, time.UTC)
	offsets := make(map[int]struct{})
	for delta := -72 * time.Hour; delta <= 72*time.Hour; delta += 30 * time.Minute {
		_, offset := wall.Add(delta).In(location).Zone()
		offsets[offset] = struct{}{}
	}
	var matches []time.Time
	for offset := range offsets {
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(location)
		if local.Year() == day.Year() && local.Month() == day.Month() && local.Day() == day.Day() && local.Hour() == minuteOfDay/60 && local.Minute() == minuteOfDay%60 && local.Second() == 0 {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		return time.Time{}, localTimeError{}
	case 1:
		return matches[0], nil
	default:
		return time.Time{}, localTimeError{ambiguous: true}
	}
}

func parseMinuteOfDay(value string) int {
	parsed, _ := time.Parse("15:04", value)
	return parsed.Hour()*60 + parsed.Minute()
}

func validateIntervals(intervals []TimeInterval) error {
	for _, interval := range intervals {
		if !interval.StartAt.Before(interval.EndAt) {
			return ErrInvalidAvailability
		}
	}
	return nil
}

func overlaps(left, right TimeInterval) bool {
	return left.StartAt.Before(right.EndAt) && right.StartAt.Before(left.EndAt)
}

func intersectsAny(candidate TimeInterval, intervals []TimeInterval) bool {
	for _, interval := range intervals {
		if overlaps(candidate, interval) {
			return true
		}
	}
	return false
}

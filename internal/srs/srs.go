// Package srs schedules the reviews of the cards: SM-2 with four grades. The functions are pure (the caller
// passes the time), so the schedule is the same for the same history and tests need no clock.
package srs

import (
	"math"
	"time"
)

// Grade is how well the user remembered a card.
type Grade int

// The grades, in the order of the buttons.
const (
	Again Grade = iota + 1
	Hard
	Good
	Easy
)

// Valid reports whether g is one of the four grades.
func (g Grade) Valid() bool { return g >= Again && g <= Easy }

// Tuning of the schedule.
const (
	InitialEase  = 2.5
	MinEase      = 1.3
	MaxDays      = 365
	graduateDays = 1
	easyDays     = 4
	hardFactor   = 1.2
	easyBonus    = 1.3
)

// learnStep is how long a card waits after Again and after Hard while it is learned, and after Again on a review
// card. Again comes back at once, Hard a bit later, so the two buttons differ. Only SM-2 uses it: FSRS computes
// the short intervals itself.
var learnStep = map[Grade]time.Duration{Again: time.Minute, Hard: 10 * time.Minute}

// LearnStep is the wait of SM-2 after g (Again or Hard) while a card is learned; 0 for the other grades.
func LearnStep(g Grade) time.Duration { return learnStep[g] }

// State is the schedule of one card. IntervalDays is 0 while the card is being learned. Stability and Difficulty
// are the FSRS memory (0: none yet) and Last is the time of the last grade (zero: never graded); SM-2 keeps them
// as they are, FSRS keeps Ease, so a card can change the algorithm and keep its history.
type State struct {
	Ease         float64
	IntervalDays int
	Reps         int
	Lapses       int
	Due          time.Time
	Learning     bool
	Stability    float64
	Difficulty   float64
	Last         time.Time
}

// New is the state of a card nobody reviewed: it is learned first. Due is zero (the card is new, not due).
func New() State { return State{Ease: InitialEase, Learning: true} }

// Next returns the state after the card was graded g at now.
func Next(s State, g Grade, now time.Time) State {
	if s.Ease < MinEase {
		s.Ease = MinEase
	}
	if s.Learning {
		switch g {
		case Good:
			return graduate(s, graduateDays, now)
		case Easy:
			return graduate(s, easyDays, now)
		default: // Again and Hard: another step, no progress
			s.Due = now.Add(learnStep[g])
			return s
		}
	}
	prev := float64(s.IntervalDays)
	switch g {
	case Again:
		s.Lapses++
		s.Ease = math.Max(MinEase, s.Ease-0.20)
		s.Learning, s.IntervalDays = true, 0
		s.Due = now.Add(learnStep[Again])
		return s
	case Hard:
		s.Ease = math.Max(MinEase, s.Ease-0.15)
		return review(s, prev*hardFactor, now)
	case Easy:
		s = review(s, prev*s.Ease*easyBonus, now)
		s.Ease += 0.15
		return s
	default: // Good
		return review(s, prev*s.Ease, now)
	}
}

func graduate(s State, days int, now time.Time) State {
	s.Learning = false
	s.Reps++
	s.IntervalDays = days
	s.Due = now.Add(days24(days))
	return s
}

// review sets the interval to the larger of raw and one day more than before, rounded up, at most MaxDays.
func review(s State, raw float64, now time.Time) State {
	days := int(math.Ceil(raw))
	if days < s.IntervalDays+1 {
		days = s.IntervalDays + 1
	}
	if days > MaxDays {
		days = MaxDays
	}
	s.Reps++
	s.IntervalDays = days
	s.Due = now.Add(days24(days))
	return s
}

func days24(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// Preview returns the time until the next review after each grade (Again, Hard, Good, Easy).
func Preview(s State, now time.Time) [4]time.Duration {
	var out [4]time.Duration
	for i, g := range []Grade{Again, Hard, Good, Easy} {
		out[i] = Next(s, g, now).Due.Sub(now)
	}
	return out
}

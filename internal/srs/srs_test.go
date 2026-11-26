package srs

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func reviewing(ease float64, days int) State {
	return State{Ease: ease, IntervalDays: days, Reps: 3, Due: t0}
}

func TestLearningPhase(t *testing.T) {
	for _, c := range []struct {
		name     string
		g        Grade
		learning bool
		days     int
		due      time.Duration
	}{
		{"again", Again, true, 0, LearnStep(Again)},
		{"hard", Hard, true, 0, LearnStep(Hard)},
		{"good", Good, false, 1, 24 * time.Hour},
		{"easy", Easy, false, 4, 4 * 24 * time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Next(New(), c.g, t0)
			if got.Learning != c.learning || got.IntervalDays != c.days || got.Due != t0.Add(c.due) {
				t.Fatalf("got %+v", got)
			}
			if got.Ease != InitialEase || got.Lapses != 0 {
				t.Fatalf("ease or lapses changed: %+v", got)
			}
		})
	}
}

func TestReviewPhase(t *testing.T) {
	for _, c := range []struct {
		name     string
		s        State
		g        Grade
		days     int
		ease     float64
		lapses   int
		learning bool
	}{
		{"again", reviewing(2.5, 10), Again, 0, 2.3, 1, true},
		{"hard", reviewing(2.5, 10), Hard, 12, 2.35, 0, false},
		{"good", reviewing(2.5, 10), Good, 25, 2.5, 0, false},
		{"easy", reviewing(2.5, 10), Easy, 33, 2.65, 0, false},
		{"good at least one more day", reviewing(1.3, 1), Good, 2, 1.3, 0, false},
		{"hard at least one more day", reviewing(2.5, 1), Hard, 2, 2.35, 0, false},
		{"ease floor on again", reviewing(1.3, 10), Again, 0, 1.3, 1, true},
		{"ease floor on hard", reviewing(1.35, 10), Hard, 12, 1.3, 0, false},
		{"cap", reviewing(2.5, 300), Good, MaxDays, 2.5, 0, false},
		{"cap on easy", reviewing(2.5, 360), Easy, MaxDays, 2.65, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Next(c.s, c.g, t0)
			if got.IntervalDays != c.days || got.Lapses != c.lapses || got.Learning != c.learning || math.Abs(got.Ease-c.ease) > 1e-9 {
				t.Fatalf("got %+v", got)
			}
			want := t0.Add(days24(c.days))
			if c.g == Again {
				want = t0.Add(LearnStep(Again))
			}
			if got.Due != want {
				t.Fatalf("due %v, want %v", got.Due, want)
			}
		})
	}
}

func TestLapseRelearnsThenGraduates(t *testing.T) {
	s := Next(reviewing(2.5, 30), Again, t0)
	s = Next(s, Good, t0.Add(LearnStep(Again)))
	if s.Learning || s.IntervalDays != 1 || s.Lapses != 1 || math.Abs(s.Ease-2.3) > 1e-9 {
		t.Fatalf("got %+v", s)
	}
}

// run applies the grades one after another, each at the time the previous one made the card due.
func run(grades ...Grade) State {
	s, now := New(), t0
	for _, g := range grades {
		s = Next(s, g, now)
		now = s.Due
	}
	return s
}

func TestHardWordsGrowSlowerThanEasyOnes(t *testing.T) {
	var hard, easy []Grade
	for i := 0; i < 8; i++ {
		hard = append(hard, Good, Again, Good, Hard)
		easy = append(easy, Good, Easy)
	}
	h, e := run(hard...), run(easy...)
	if h.IntervalDays >= e.IntervalDays {
		t.Fatalf("hard %d days, easy %d days", h.IntervalDays, e.IntervalDays)
	}
	if h.Ease >= e.Ease || h.Lapses == 0 {
		t.Fatalf("hard %+v, easy %+v", h, e)
	}
	// the easy word spaces out: every review waits longer than the one before
	s, now, prev := New(), t0, 0
	for i := 0; i < 6; i++ {
		s = Next(s, Good, now)
		now = s.Due
		if s.IntervalDays <= prev {
			t.Fatalf("interval %d did not grow after %d", s.IntervalDays, prev)
		}
		prev = s.IntervalDays
	}
}

func TestPreview(t *testing.T) {
	got := Preview(New(), t0)
	want := [4]time.Duration{LearnStep(Again), LearnStep(Hard), 24 * time.Hour, 4 * 24 * time.Hour}
	if got != want {
		t.Fatalf("got %v", got)
	}
	got = Preview(reviewing(2.5, 10), t0)
	want = [4]time.Duration{LearnStep(Again), 12 * 24 * time.Hour, 25 * 24 * time.Hour, 33 * 24 * time.Hour}
	if got != want {
		t.Fatalf("got %v", got)
	}
}

func TestGradeValid(t *testing.T) {
	for g, want := range map[Grade]bool{0: false, Again: true, Easy: true, 5: false} {
		if g.Valid() != want {
			t.Fatalf("%d", g)
		}
	}
}

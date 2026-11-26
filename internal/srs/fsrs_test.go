package srs

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func fsrsSched() Scheduler { return Scheduler{Algo: FSRS} }

func TestFSRSFirstGradeUsesInitialStability(t *testing.T) {
	sc := fsrsSched()
	for g, days := range map[Grade]int{Hard: 1, Good: 2, Easy: 8} {
		got := sc.Next(New(), g, t0)
		if got.Learning || got.IntervalDays != days || got.Due != t0.Add(days24(days)) {
			t.Fatalf("grade %d: %+v", g, got)
		}
		if math.Abs(got.Stability-DefaultWeights[g-1]) > 1e-9 {
			t.Fatalf("grade %d: stability %v", g, got.Stability)
		}
	}
	d := sc.Next(New(), Again, t0).Difficulty
	want := DefaultWeights[4] - math.Exp(0) + 1
	if math.Abs(d-math.Min(10, math.Max(1, want))) > 1e-9 {
		t.Fatalf("D0(Again) %v", d)
	}
}

func TestFSRSAgainReturnsWithinTenMinutes(t *testing.T) {
	sc := fsrsSched()
	got := sc.Next(New(), Again, t0)
	if !got.Learning || got.Due != t0.Add(10*time.Minute) {
		t.Fatalf("%+v", got)
	}
	s := sc.Next(New(), Good, t0)
	s = sc.Next(s, Good, s.Due)
	lapsed := sc.Next(s, Again, s.Due)
	if lapsed.Lapses != 1 || !lapsed.Learning || lapsed.IntervalDays != 0 || lapsed.Due.Sub(s.Due) > 10*time.Minute {
		t.Fatalf("%+v", lapsed)
	}
	if lapsed.Stability >= s.Stability {
		t.Fatalf("stability %v did not drop from %v", lapsed.Stability, s.Stability)
	}
}

func TestFSRSKnownSequence(t *testing.T) {
	// Good, Good: S1 = 2.3065, then after 2 days R = (1+f*2/S)^-w20 and the recall formula
	w := DefaultWeights
	sc := fsrsSched()
	s := sc.Next(New(), Good, t0)
	d1 := math.Min(10, math.Max(1, w[4]-math.Exp(w[5]*2)+1))
	s2 := sc.Next(s, Good, s.Due)
	f := math.Pow(0.9, -1/w[20]) - 1
	r := math.Pow(1+f*2/w[2], -w[20])
	want := w[2] * (1 + math.Exp(w[8])*(11-d1)*math.Pow(w[2], -w[9])*(math.Exp(w[10]*(1-r))-1))
	if math.Abs(s2.Stability-want) > 1e-9 {
		t.Fatalf("stability %v, want %v", s2.Stability, want)
	}
	if s2.Learning || s2.Reps != 2 || s2.IntervalDays != int(math.Round(want)) {
		t.Fatalf("%+v", s2)
	}
}

func TestFSRSSameDayGrowsUntilGraduation(t *testing.T) {
	sc := fsrsSched()
	s := sc.Next(New(), Again, t0)
	prev := s.Stability
	now := s.Due
	for i := 0; i < 40 && s.Learning; i++ {
		s = sc.Next(s, Good, now)
		if s.Stability <= prev {
			t.Fatalf("stability %v did not grow from %v", s.Stability, prev)
		}
		prev = s.Stability
		if s.Learning && s.Due.Sub(now) < time.Minute {
			t.Fatalf("interval below a minute: %v", s.Due.Sub(now))
		}
		now = s.Due
	}
	if s.Learning {
		t.Fatalf("never graduated: %+v", s)
	}
}

func TestFSRSIntervalsAreMonotonicInGrade(t *testing.T) {
	sc := fsrsSched()
	s := sc.Next(New(), Good, t0)
	now := s.Due
	p := sc.Preview(s, now)
	if !(p[0] < p[1] && p[1] < p[2] && p[2] < p[3]) {
		t.Fatalf("%v", p)
	}
	p = sc.Preview(New(), t0)
	if !(p[0] < p[1] && p[1] < p[2] && p[2] < p[3]) {
		t.Fatalf("%v", p)
	}
}

func TestFSRSHigherRetentionMeansShorterIntervals(t *testing.T) {
	lo, hi := Scheduler{Algo: FSRS, Retention: 0.8}, Scheduler{Algo: FSRS, Retention: 0.95}
	a, b := lo.Next(New(), Good, t0), hi.Next(New(), Good, t0)
	if !(b.Due.Before(a.Due)) {
		t.Fatalf("0.95: %v, 0.8: %v", b.Due, a.Due)
	}
	// out of range is clamped
	if (Scheduler{Retention: 2}).retention() != MaxRetention || (Scheduler{Retention: 0.1}).retention() != MinRetention {
		t.Fatal("retention not clamped")
	}
}

func TestFSRSCapsAtMaxDays(t *testing.T) {
	sc := fsrsSched()
	s := State{Ease: 2.5, IntervalDays: 300, Reps: 5, Stability: 20000, Difficulty: 3, Last: t0.Add(-300 * 24 * time.Hour), Due: t0}
	got := sc.Next(s, Easy, t0)
	if got.IntervalDays != MaxDays {
		t.Fatalf("%+v", got)
	}
}

func TestFSRSMinimumIntervalIsOneMinute(t *testing.T) {
	w := DefaultWeights
	w[0] = 0.0001 // Again: stability of a few seconds
	got := Scheduler{Algo: FSRS, Weights: w}.Next(New(), Again, t0)
	if got.Due.Sub(t0) != time.Minute {
		t.Fatalf("%v", got.Due.Sub(t0))
	}
}

func TestFSRSStartsFromSM2Card(t *testing.T) {
	sc := fsrsSched()
	s := State{Ease: 1.3, IntervalDays: 20, Reps: 4, Due: t0, Last: t0.Add(-20 * 24 * time.Hour)}
	got := sc.Next(s, Good, t0)
	if got.Ease != 1.3 || got.Stability <= 20 || got.Difficulty < 9.9 || got.Learning {
		t.Fatalf("%+v", got)
	}
	// ease 2.5 -> difficulty 5
	m, has, _ := memoryOf(State{Ease: 2.5, IntervalDays: 5, Last: t0}, t0)
	if !has || math.Abs(float64(m.d)-5) > 1e-9 {
		t.Fatalf("%v", m)
	}
	// a card SM-2 is learning has no memory
	if _, has, _ := memoryOf(State{Ease: 2.5, Learning: true, Reps: 2}, t0); has {
		t.Fatal("learning card has memory")
	}
}

func TestSchedulerSM2IsUnchanged(t *testing.T) {
	if (Scheduler{}).Next(New(), Good, t0) != Next(New(), Good, t0) {
		t.Fatal("zero Scheduler is not SM-2")
	}
	if (Scheduler{}).Preview(New(), t0) != Preview(New(), t0) {
		t.Fatal("preview differs")
	}
}

// the Var forward must agree with the float forward
func TestVarForwardMatchesFloat(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 50; i++ {
		tape := NewTape()
		wf := make([]F, NumWeights)
		wv := make([]Var, NumWeights)
		for j := range wf {
			x := DefaultWeights[j] * (0.7 + 0.6*rng.Float64())
			x = math.Min(math.Max(x, weightBounds[j][0]), weightBounds[j][1])
			wf[j], wv[j] = F(x), tape.Leaf(x)
		}
		mf, mv := memory[F]{}, memory[Var]{}
		first := true
		for k := 0; k < 8; k++ {
			g := Grade(1 + rng.IntN(4))
			el := rng.Float64() * 20
			mf, mv = step(wf, mf, first, g, el), step(wv, mv, first, g, el)
			first = false
			if math.Abs(float64(mf.s)-mv.s.Val()) > 1e-9 || math.Abs(float64(mf.d)-mv.d.Val()) > 1e-9 {
				t.Fatalf("step %d: %v vs %v", k, mf, mv)
			}
		}
	}
}

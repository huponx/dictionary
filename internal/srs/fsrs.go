package srs

import (
	"math"
	"time"
)

// Algorithm is the scheduling algorithm a user chose.
type Algorithm string

// The algorithms. The empty string means SM2.
const (
	SM2  Algorithm = "sm2"
	FSRS Algorithm = "fsrs"
)

// NumWeights is the number of parameters of FSRS-6.
const NumWeights = 21

// Weights are the parameters of FSRS-6.
type Weights [NumWeights]float64

// DefaultWeights are the parameters of the reference implementation (open-spaced-repetition, FSRS-6).
var DefaultWeights = Weights{
	0.212, 1.2931, 2.3065, 8.2956, 6.4133, 0.8334, 3.0194, 0.001, 1.8722, 0.1666, 0.796,
	1.4835, 0.0614, 0.2629, 1.6483, 0.6014, 1.8729, 0.5425, 0.0912, 0.0658, 0.1542,
}

const sMin = 0.001 // smallest stability in days

// weightBounds are the range of each parameter; the optimizer keeps them inside.
var weightBounds = [NumWeights][2]float64{
	{sMin, 100}, {sMin, 100}, {sMin, 100}, {sMin, 100}, {1, 10}, {0.001, 4}, {0.001, 4}, {0.001, 0.75},
	{0, 4.5}, {0, 0.8}, {0.001, 3.5}, {0.001, 5}, {0.001, 0.25}, {0.001, 0.9}, {0, 4}, {0, 1}, {1, 6},
	{0, 2}, {0, 2}, {0, 0.8}, {0.1, 0.8},
}

// Tuning of the FSRS schedule.
const (
	DefaultRetention = 0.90
	MinRetention     = 0.70
	MaxRetention     = 0.97
	minInterval      = time.Minute
	// againCap is the longest an Again waits: the card must come back in the same session, while the stability
	// after Again (5 hours with the default weights) is too far for that.
	againCap = 10 * time.Minute
	maxStab  = 36500.0
)

// Scheduler picks the next review of a card with the algorithm of the user. The zero value is SM-2.
type Scheduler struct {
	Algo Algorithm
	// Retention is the probability of remembering a card on the day it is due (FSRS): 0.70 to 0.97, 0 for 0.90.
	Retention float64
	// Weights are the FSRS parameters; all zero for DefaultWeights.
	Weights Weights
}

func (sc Scheduler) retention() float64 {
	switch {
	case sc.Retention == 0:
		return DefaultRetention
	case sc.Retention < MinRetention:
		return MinRetention
	case sc.Retention > MaxRetention:
		return MaxRetention
	}
	return sc.Retention
}

func (sc Scheduler) weights() Weights {
	if sc.Weights == (Weights{}) {
		return DefaultWeights
	}
	return sc.Weights
}

// Next returns the state after the card was graded g at now.
func (sc Scheduler) Next(s State, g Grade, now time.Time) State {
	if sc.Algo != FSRS {
		return Next(s, g, now)
	}
	return sc.fsrs(s, g, now)
}

// Preview returns the time until the next review after each grade (Again, Hard, Good, Easy).
func (sc Scheduler) Preview(s State, now time.Time) [4]time.Duration {
	var out [4]time.Duration
	for i, g := range []Grade{Again, Hard, Good, Easy} {
		out[i] = sc.Next(s, g, now).Due.Sub(now)
	}
	return out
}

func toF(w Weights) []F {
	out := make([]F, NumWeights)
	for i, x := range w {
		out[i] = F(x)
	}
	return out
}

// memory is the FSRS state of a card.
type memory[T Num[T]] struct{ s, d T }

func neg[T Num[T]](x T) T { return x.Const(0).Sub(x) }

func clamp[T Num[T]](x T, lo, hi float64) T { return x.Max(x.Const(lo)).Min(x.Const(hi)) }

// factor of the forgetting curve R(t,S) = (1 + factor*t/S)^-w20, which makes R(S,S) = 0.9.
func factor[T Num[T]](w []T) T {
	one := w[0].Const(1)
	return w[0].Const(0.9).Pow(neg(one.Div(w[20]))).Sub(one)
}

// retrievability is the probability of remembering after t days with stability s.
func retrievability[T Num[T]](w []T, t, s T) T {
	one := t.Const(1)
	return one.Add(factor(w).Mul(t).Div(s)).Pow(neg(w[20]))
}

// intervalDays is the number of days after which the retrievability falls to r.
func intervalDays[T Num[T]](w []T, s T, r float64) T {
	one := s.Const(1)
	return s.Div(factor(w)).Mul(s.Const(r).Pow(neg(one.Div(w[20]))).Sub(one))
}

func initStability[T Num[T]](w []T, g Grade) T { return w[g-1] }

// initDifficulty is D0(g); only the first grade of a card is clamped to 1..10.
func initDifficulty[T Num[T]](w []T, g Grade, doClamp bool) T {
	d := w[4].Sub(w[5].Mul(w[4].Const(float64(g - 1))).Exp()).Add(w[4].Const(1))
	if doClamp {
		return clamp(d, 1, 10)
	}
	return d
}

// nextDifficulty moves d by the grade, linearly damped near 10, and pulls it towards D0(Easy).
func nextDifficulty[T Num[T]](w []T, d T, g Grade) T {
	delta := neg(w[6]).Mul(d.Const(float64(g - 3)))
	moved := d.Add(d.Const(10).Sub(d).Mul(delta).Div(d.Const(9)))
	one := d.Const(1)
	return clamp(w[7].Mul(initDifficulty(w, Easy, false)).Add(one.Sub(w[7]).Mul(moved)), 1, 10)
}

func stabilityRecall[T Num[T]](w []T, m memory[T], r T, g Grade) T {
	one := m.s.Const(1)
	f := w[8].Exp().Mul(m.s.Const(11).Sub(m.d)).Mul(m.s.Pow(neg(w[9]))).Mul(w[10].Mul(one.Sub(r)).Exp().Sub(one))
	switch g {
	case Hard:
		f = f.Mul(w[15])
	case Easy:
		f = f.Mul(w[16])
	}
	return clamp(m.s.Mul(one.Add(f)), sMin, maxStab)
}

func stabilityForget[T Num[T]](w []T, m memory[T], r T) T {
	one := m.s.Const(1)
	f := w[11].Mul(m.d.Pow(neg(w[12]))).Mul(m.s.Add(one).Pow(w[13]).Sub(one)).Mul(w[14].Mul(one.Sub(r)).Exp())
	return clamp(f.Min(m.s.Div(w[17].Mul(w[18]).Exp())), sMin, maxStab)
}

// stabilitySameDay is the stability after a grade less than a day after the previous one.
func stabilitySameDay[T Num[T]](w []T, s T, g Grade) T {
	inc := w[17].Mul(w[18].Add(s.Const(float64(g - 3)))).Exp().Mul(s.Pow(neg(w[19])))
	if g >= Good {
		inc = inc.Max(s.Const(1))
	}
	return clamp(s.Mul(inc), sMin, maxStab)
}

// step is the memory after grade g, elapsed days after the previous one. first: there is no previous grade.
func step[T Num[T]](w []T, m memory[T], first bool, g Grade, elapsed float64) memory[T] {
	if first {
		return memory[T]{s: clamp(initStability(w, g), sMin, maxStab), d: initDifficulty(w, g, true)}
	}
	d := nextDifficulty(w, m.d, g)
	if elapsed < 1 {
		return memory[T]{s: stabilitySameDay(w, m.s, g), d: d}
	}
	r := retrievability(w, m.s.Const(elapsed), m.s)
	if g == Again {
		return memory[T]{s: stabilityForget(w, m, r), d: d}
	}
	return memory[T]{s: stabilityRecall(w, m, r, g), d: d}
}

// memoryOf is the memory of a card before a grade at now, whether there is one, and the days since the last
// grade. A card that SM-2 graduated has none: it starts with its interval and a difficulty from its ease (2.5 is
// 5, 1.3 is 10). A card SM-2 is still learning has none either, so its next grade is its first.
func memoryOf(s State, now time.Time) (m memory[F], has bool, elapsed float64) {
	switch {
	case s.Stability > 0:
		m = memory[F]{F(s.Stability), F(s.Difficulty)}
	case s.Learning:
		return m, false, 0
	default:
		ease := s.Ease
		if ease <= 0 {
			ease = InitialEase
		}
		d := 5 + (InitialEase-ease)*(5/(InitialEase-MinEase))
		m = memory[F]{F(max(float64(s.IntervalDays), 1)), F(math.Min(10, math.Max(1, d)))}
	}
	if s.Last.IsZero() {
		return m, true, float64(m.s)
	}
	return m, true, math.Max(0, now.Sub(s.Last).Hours()/24)
}

func (sc Scheduler) fsrs(s State, g Grade, now time.Time) State {
	w := toF(sc.weights())
	m, has, elapsed := memoryOf(s, now)
	nm := step(w, m, !has, g, elapsed)
	s.Stability, s.Difficulty, s.Last = float64(nm.s), float64(nm.d), now
	if s.Ease < MinEase {
		s.Ease = InitialEase
	}
	wasLearning := s.Learning
	days := float64(intervalDays(w, nm.s, sc.retention()))
	if g == Again {
		if !wasLearning {
			s.Lapses++
		}
		s.Learning, s.IntervalDays = true, 0
		s.Due = now.Add(roundMinute(min(againCap, max(minInterval, daysDur(days)))))
		return s
	}
	if !wasLearning {
		days = math.Max(days, 1) // a review card does not go back to short steps without an Again
	}
	if days >= 1 {
		n := int(math.Min(math.Max(math.Round(days), 1), MaxDays))
		s.Learning, s.IntervalDays = false, n
		s.Reps++
		s.Due = now.Add(days24(n))
		return s
	}
	s.Learning, s.IntervalDays = true, 0
	s.Due = now.Add(roundMinute(max(minInterval, daysDur(days))))
	return s
}

func daysDur(days float64) time.Duration { return time.Duration(days * float64(24*time.Hour)) }

func roundMinute(d time.Duration) time.Duration { return d.Round(time.Minute) }

package srs

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"sort"
)

// Review is one grade of a card in the log. Elapsed is the seconds since the previous grade of the card (0 for the
// first); At is the time of the grade in Unix seconds.
type Review struct {
	Grade   Grade
	At      int64
	Elapsed int64
}

// Tuning of the optimizer.
const (
	// MinSamples is how many grades after a gap of a day or more a user needs before the weights are fitted.
	MinSamples = 200
	// MaxSamples bounds the data the caller feeds: the most recent grades.
	MaxSamples = 50000

	holdoutShare = 0.2
	minHoldout   = 20
	batchCards   = 128
	minEpochs    = 5
	maxEpochs    = 100
	targetSteps  = 300
	baseRate     = 0.04
	regWeight    = 0.05
	dayInSeconds = 86400
)

// ErrNotEnoughData is returned when the log has fewer than MinSamples useful grades.
var ErrNotEnoughData = errors.New("srs: not enough reviews to optimize")

// OptimizeResult is what Optimize found. Weights are the fitted ones even when they were not accepted.
type OptimizeResult struct {
	Weights Weights
	// LossDefault and LossTrained are the log-loss of the default and the fitted weights on the held-out 20%.
	LossDefault, LossTrained float64
	// Samples is the number of graded recalls the fit used (training and held-out).
	Samples int
	// Applied: the fitted weights beat the defaults on the held-out part. Reason says why not.
	Applied bool
	Reason  string
}

// isSample: a grade enters the loss when it is not the first of the card and came a day or more after the previous
// one. Grades within the day still move the memory.
func isSample(i int, r Review) bool { return i > 0 && r.Elapsed >= dayInSeconds }

func chainLoss[T Num[T]](w []T, c []Review, after int64) (sum T, n int) {
	sum = w[0].Const(0)
	var m memory[T]
	for i, r := range c {
		elapsed := float64(r.Elapsed) / dayInSeconds
		if isSample(i, r) && r.At > after {
			p := clamp(retrievability(w, w[0].Const(elapsed), m.s), 1e-6, 1-1e-6)
			one := p.Const(1)
			if r.Grade > Again {
				sum = sum.Sub(p.Log())
			} else {
				sum = sum.Sub(one.Sub(p).Log())
			}
			n++
		}
		m = step(w, m, i == 0, r.Grade, elapsed)
	}
	return sum, n
}

// cardsLoss is the mean log-loss of the weights over the samples after the time `after`.
func cardsLoss(cards [][]Review, w Weights, after int64) (float64, int) {
	wf := toF(w)
	var sum float64
	var n int
	for _, c := range cards {
		s, k := chainLoss(wf, c, after)
		sum += float64(s)
		n += k
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

// cardsLossVar records the summed log-loss of the cards on the tape.
func cardsLossVar(t *Tape, cards [][]Review, w []Var, after int64) (Var, int) {
	sum, n := w[0].Const(0), 0
	for _, c := range cards {
		s, k := chainLoss(w, c, after)
		sum = sum.Add(s)
		n += k
	}
	return sum, n
}

func countSamples(cards [][]Review) int {
	n := 0
	for _, c := range cards {
		for i, r := range c {
			if isSample(i, r) {
				n++
			}
		}
	}
	return n
}

// Optimize fits the FSRS weights to the history of one user (chains of grades per card, each in time order, the
// cards ordered by their first grade). The first 80% of the samples in time train, the last 20% judge: the fitted
// weights are only Applied when they beat the defaults there. The result is the same for the same input.
func Optimize(ctx context.Context, cards [][]Review) (OptimizeResult, error) {
	res := OptimizeResult{Weights: DefaultWeights}
	res.Samples = countSamples(cards)
	if res.Samples < MinSamples {
		return res, ErrNotEnoughData
	}
	var times []int64
	for _, c := range cards {
		for i, r := range c {
			if isSample(i, r) {
				times = append(times, r.At)
			}
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	cutoff := times[int(float64(len(times))*(1-holdoutShare))-1]
	var train [][]Review
	for _, c := range cards {
		k := sort.Search(len(c), func(i int) bool { return c[i].At > cutoff })
		if k > 1 {
			train = append(train, c[:k])
		}
	}
	if _, n := cardsLoss(cards, DefaultWeights, cutoff); n < minHoldout {
		res.Reason = "not enough recent reviews to check the result"
		return res, nil
	}

	w := DefaultWeights
	var m, v [NumWeights]float64
	rng := rand.New(rand.NewPCG(1, 2))
	order := rng.Perm(len(train))
	batches := (len(train) + batchCards - 1) / batchCards
	epochs := min(maxEpochs, max(minEpochs, (targetSteps+batches-1)/batches))
	steps := 0
	total := epochs * batches
	const b1, b2, eps = 0.9, 0.999, 1e-8
	for e := 0; e < epochs; e++ {
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for b := 0; b < batches; b++ {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			batch := make([][]Review, 0, batchCards)
			for _, i := range order[b*batchCards : min(len(order), (b+1)*batchCards)] {
				batch = append(batch, train[i])
			}
			tape := NewTape()
			leaves := make([]Var, NumWeights)
			for i := range leaves {
				leaves[i] = tape.Leaf(w[i])
			}
			sum, n := cardsLossVar(tape, batch, leaves, 0)
			if n == 0 {
				continue
			}
			adj := tape.Backward(sum.Div(sum.Const(float64(n))))
			steps++
			// cosine decay of the step size
			rate := baseRate * 0.5 * (1 + math.Cos(math.Pi*float64(steps)/float64(total+1)))
			for i := range w {
				rng := weightBounds[i][1] - weightBounds[i][0]
				g := adj[leaves[i].Index()] + 2*regWeight*(w[i]-DefaultWeights[i])/(rng*rng)
				m[i] = b1*m[i] + (1-b1)*g
				v[i] = b2*v[i] + (1-b2)*g*g
				mh, vh := m[i]/(1-math.Pow(b1, float64(steps))), v[i]/(1-math.Pow(b2, float64(steps)))
				w[i] -= rate * math.Min(rng, 5) * mh / (math.Sqrt(vh) + eps)
				w[i] = math.Min(math.Max(w[i], weightBounds[i][0]), weightBounds[i][1])
			}
		}
	}
	res.Weights = w
	res.LossDefault, _ = cardsLoss(cards, DefaultWeights, cutoff)
	res.LossTrained, _ = cardsLoss(cards, w, cutoff)
	if res.LossTrained < res.LossDefault {
		res.Applied = true
	} else {
		res.Reason = "the fitted parameters are no better than the defaults"
	}
	return res, nil
}

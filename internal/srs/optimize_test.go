package srs

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"
)

// simulate makes the history of a user whose memory follows w: each card is reviewed after growing gaps and
// recalled with the probability of the forgetting curve.
func simulate(w Weights, cards int, seed uint64) [][]Review {
	rng := rand.New(rand.NewPCG(seed, 99))
	wf := toF(w)
	var out [][]Review
	at := int64(1_700_000_000)
	for i := 0; i < cards; i++ {
		start := at + int64(i)*600
		var c []Review
		var m memory[F]
		g := Grade(1 + rng.IntN(4))
		now := start
		c = append(c, Review{Grade: g, At: now})
		m = step(wf, m, true, g, 0)
		for k := 0; k < 8; k++ {
			gapDays := math.Max(1, float64(m.s)*(0.5+rng.Float64()*1.5))
			el := int64(gapDays * dayInSeconds)
			now += el
			r := retrievability(wf, F(gapDays), m.s)
			g = Again
			if rng.Float64() < float64(r) {
				g = Grade(2 + rng.IntN(3))
			}
			c = append(c, Review{Grade: g, At: now, Elapsed: el})
			m = step(wf, m, false, g, gapDays)
		}
		out = append(out, c)
	}
	return out
}

func TestOptimizeBeatsDefaultsOnDifferentUser(t *testing.T) {
	truth := DefaultWeights
	truth[2] *= 0.5  // forgets faster than the average
	truth[8] *= 0.6  // grows stability slower
	truth[20] = 0.35 // other curve
	data := simulate(truth, 1500, 1)
	res, err := Optimize(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.LossTrained >= res.LossDefault {
		t.Fatalf("not applied: %+v", res)
	}
	// on fresh data of the same user the fitted weights predict better, and better than the defaults by a margin
	fresh := simulate(truth, 800, 77)
	lt, _ := cardsLoss(fresh, res.Weights, 0)
	ld, _ := cardsLoss(fresh, DefaultWeights, 0)
	lo, _ := cardsLoss(fresh, truth, 0)
	t.Logf("fresh loss: default %.4f fitted %.4f truth %.4f", ld, lt, lo)
	if lt >= ld {
		t.Fatalf("fitted %v not better than default %v", lt, ld)
	}
	for i, x := range res.Weights {
		if x < weightBounds[i][0] || x > weightBounds[i][1] {
			t.Fatalf("w%d=%v out of bounds", i, x)
		}
	}
	again, _ := Optimize(context.Background(), data)
	if again.Weights != res.Weights {
		t.Fatal("not deterministic")
	}
}

func TestOptimizeRejectsLittleData(t *testing.T) {
	_, err := Optimize(context.Background(), simulate(DefaultWeights, 10, 3))
	if err != ErrNotEnoughData {
		t.Fatalf("err %v", err)
	}
}

func TestOptimizeHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Optimize(ctx, simulate(DefaultWeights, 300, 4)); err == nil {
		t.Fatal("no error")
	}
}

func BenchmarkOptimize50k(b *testing.B) {
	data := simulate(DefaultWeights, 6500, 5) // about 50,000 samples
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Optimize(context.Background(), data); err != nil {
			b.Fatal(err)
		}
	}
}

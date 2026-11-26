package srs

import (
	"math"
	"math/rand/v2"
	"testing"
)

func numGrad(f func(x []float64) float64, x []float64, i int) float64 {
	h := 1e-6 * math.Max(1, math.Abs(x[i]))
	a, b := append([]float64(nil), x...), append([]float64(nil), x...)
	a[i] += h
	b[i] -= h
	return (f(a) - f(b)) / (2 * h)
}

func closeRel(a, b float64) bool {
	return math.Abs(a-b) <= 1e-4*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func TestOpsGradients(t *testing.T) {
	ops := map[string]func(a, b Var) Var{
		"add": Var.Add, "sub": Var.Sub, "mul": Var.Mul, "div": Var.Div, "pow": Var.Pow, "min": Var.Min, "max": Var.Max,
		"exp":   func(a, b Var) Var { return a.Exp().Mul(b) },
		"log":   func(a, b Var) Var { return a.Log().Mul(b) },
		"chain": func(a, b Var) Var { return a.Mul(b).Exp().Add(a.Div(b)).Sub(b.Pow(a.Const(2))) },
	}
	for name, op := range ops {
		x := []float64{1.7, 0.6}
		eval := func(x []float64) float64 {
			tp := NewTape()
			return op(tp.Leaf(x[0]), tp.Leaf(x[1])).Val()
		}
		tp := NewTape()
		a, b := tp.Leaf(x[0]), tp.Leaf(x[1])
		adj := tp.Backward(op(a, b))
		for i, v := range []Var{a, b} {
			if want := numGrad(eval, x, i); !closeRel(adj[v.Index()], want) {
				t.Errorf("%s d/dx%d: %v, want %v", name, i, adj[v.Index()], want)
			}
		}
	}
}

func TestConstantsRecordNothing(t *testing.T) {
	c := Var{}.Const(2).Mul(Var{}.Const(3))
	if c.Index() != -1 || c.Val() != 6 {
		t.Fatalf("%+v", c)
	}
}

// randomCards makes review chains: 1 to 8 grades with gaps of hours to months.
func randomCards(rng *rand.Rand, n int) [][]Review {
	base := int64(1_700_000_000)
	var cards [][]Review
	for i := 0; i < n; i++ {
		at := base + rng.Int64N(1_000_000)
		var c []Review
		for k, m := 0, 2+rng.IntN(7); k < m; k++ {
			gap := int64(0)
			if k > 0 {
				gap = int64(3600 * (1 + rng.IntN(24*40)))
			}
			at += gap
			c = append(c, Review{Grade: Grade(1 + rng.IntN(4)), At: at, Elapsed: gap})
		}
		cards = append(cards, c)
	}
	return cards
}

func TestLossGradientMatchesFiniteDifferences(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	cards := randomCards(rng, 12)
	x := append([]float64(nil), DefaultWeights[:]...)
	for i := range x {
		x[i] *= 0.9 + 0.2*rng.Float64()
	}
	lossF := func(x []float64) float64 {
		var w Weights
		copy(w[:], x)
		l, _ := cardsLoss(cards, w, 0)
		return l
	}
	tp := NewTape()
	leaves := make([]Var, NumWeights)
	for i := range leaves {
		leaves[i] = tp.Leaf(x[i])
	}
	loss, n := cardsLossVar(tp, cards, leaves, 0)
	if n == 0 {
		t.Fatal("no samples")
	}
	adj := tp.Backward(loss.Div(loss.Const(float64(n))))
	for i := range x {
		if want := numGrad(lossF, x, i); !closeRel(adj[leaves[i].Index()], want) {
			t.Errorf("w%d: %v, want %v", i, adj[leaves[i].Index()], want)
		}
	}
}

package srs

import "math"

// Num is the arithmetic the FSRS formulas use. F computes the numbers only (the scheduler), Var also records the
// operations on a Tape so the optimizer gets the derivatives (reverse-mode autodiff). One generic formula serves both.
type Num[T any] interface {
	Val() float64
	Const(x float64) T
	Add(T) T
	Sub(T) T
	Mul(T) T
	Div(T) T
	Exp() T
	Log() T
	Pow(T) T
	Min(T) T
	Max(T) T
}

// F is a plain number.
type F float64

// Val returns the number.
func (a F) Val() float64 { return float64(a) }

// Const returns x.
func (F) Const(x float64) F { return F(x) }

// Add returns a+b.
func (a F) Add(b F) F { return a + b }

// Sub returns a-b.
func (a F) Sub(b F) F { return a - b }

// Mul returns a*b.
func (a F) Mul(b F) F { return a * b }

// Div returns a/b.
func (a F) Div(b F) F { return a / b }

// Exp returns e^a.
func (a F) Exp() F { return F(math.Exp(float64(a))) }

// Log returns ln a.
func (a F) Log() F { return F(math.Log(float64(a))) }

// Pow returns a^b.
func (a F) Pow(b F) F { return F(math.Pow(float64(a), float64(b))) }

// Min returns the smaller of a and b.
func (a F) Min(b F) F { return F(math.Min(float64(a), float64(b))) }

// Max returns the larger of a and b.
func (a F) Max(b F) F { return F(math.Max(float64(a), float64(b))) }

// node is one recorded operation: its inputs (-1: none) and the partial derivatives of the result by them.
type node struct {
	a, b   int32
	da, db float64
}

// Tape records the operations of one forward pass.
type Tape struct{ nodes []node }

// NewTape returns an empty tape.
func NewTape() *Tape { return &Tape{} }

// Len is the number of recorded nodes (what the memory of the pass grows with).
func (t *Tape) Len() int { return len(t.nodes) }

// Var is a number on a Tape. A constant (no tape) records nothing.
type Var struct {
	t *Tape
	i int32
	v float64
}

// Leaf adds an input to the tape.
func (t *Tape) Leaf(x float64) Var {
	t.nodes = append(t.nodes, node{a: -1, b: -1})
	return Var{t, int32(len(t.nodes) - 1), x}
}

// Val returns the number.
func (a Var) Val() float64 { return a.v }

// Const returns a constant: it has no derivative.
func (Var) Const(x float64) Var { return Var{i: -1, v: x} }

func (a Var) un(v, da float64) Var {
	if a.t == nil {
		return Var{i: -1, v: v}
	}
	a.t.nodes = append(a.t.nodes, node{a: a.i, b: -1, da: da})
	return Var{a.t, int32(len(a.t.nodes) - 1), v}
}

func (a Var) bin(b Var, v, da, db float64) Var {
	t := a.t
	if t == nil {
		t = b.t
	}
	if t == nil {
		return Var{i: -1, v: v}
	}
	n := node{a: a.i, b: b.i, da: da, db: db}
	if a.t == nil {
		n.a = -1
	}
	if b.t == nil {
		n.b = -1
	}
	t.nodes = append(t.nodes, n)
	return Var{t, int32(len(t.nodes) - 1), v}
}

// Add returns a+b.
func (a Var) Add(b Var) Var { return a.bin(b, a.v+b.v, 1, 1) }

// Sub returns a-b.
func (a Var) Sub(b Var) Var { return a.bin(b, a.v-b.v, 1, -1) }

// Mul returns a*b.
func (a Var) Mul(b Var) Var { return a.bin(b, a.v*b.v, b.v, a.v) }

// Div returns a/b.
func (a Var) Div(b Var) Var { return a.bin(b, a.v/b.v, 1/b.v, -a.v/(b.v*b.v)) }

// Exp returns e^a.
func (a Var) Exp() Var { e := math.Exp(a.v); return a.un(e, e) }

// Log returns ln a.
func (a Var) Log() Var { return a.un(math.Log(a.v), 1/a.v) }

// Pow returns a^b (a > 0 where b has a derivative).
func (a Var) Pow(b Var) Var {
	p := math.Pow(a.v, b.v)
	da := b.v * math.Pow(a.v, b.v-1)
	db := 0.0
	if b.t != nil && a.v > 0 {
		db = p * math.Log(a.v)
	}
	return a.bin(b, p, da, db)
}

// Min returns the smaller of a and b; the derivative follows it.
func (a Var) Min(b Var) Var {
	if a.v <= b.v {
		return a.bin(b, a.v, 1, 0)
	}
	return a.bin(b, b.v, 0, 1)
}

// Max returns the larger of a and b; the derivative follows it.
func (a Var) Max(b Var) Var {
	if a.v >= b.v {
		return a.bin(b, a.v, 1, 0)
	}
	return a.bin(b, b.v, 0, 1)
}

// Backward returns the derivative of out by every node of the tape (index by Var.Index).
func (t *Tape) Backward(out Var) []float64 {
	adj := make([]float64, len(t.nodes))
	if out.t != t {
		return adj
	}
	adj[out.i] = 1
	for i := len(t.nodes) - 1; i >= 0; i-- {
		g := adj[i]
		if g == 0 {
			continue
		}
		n := t.nodes[i]
		if n.a >= 0 {
			adj[n.a] += g * n.da
		}
		if n.b >= 0 {
			adj[n.b] += g * n.db
		}
	}
	return adj
}

// Index is the position of the variable on its tape, to read its derivative from Backward; -1 for a constant.
func (a Var) Index() int { return int(a.i) }

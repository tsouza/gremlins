/*
 * Copyright 2022 The Gremlins Authors
 *
 *    Licensed under the Apache License, Version 2.0 (the "License");
 *    you may not use this file except in compliance with the License.
 *    You may obtain a copy of the License at
 *
 *        http://www.apache.org/licenses/LICENSE-2.0
 *
 *    Unless required by applicable law or agreed to in writing, software
 *    distributed under the License is distributed on an "AS IS" BASIS,
 *    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *    See the License for the specific language governing permissions and
 *    limitations under the License.
 */

package engine_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tsouza/gremlins/internal/configuration"
	"github.com/tsouza/gremlins/internal/engine"
	"github.com/tsouza/gremlins/internal/gomodule"
	"github.com/tsouza/gremlins/internal/mutator"
)

// Go spells `+`, `-`, `&` and `^` the same in prefix and infix position and
// means something different by each. Mutating a prefix `&` as if it were
// bitwise AND turns `&Foo{}` into `|Foo{}`, which does not parse; mutating a
// prefix `^` the same way turns `^u` into `&u`, which no longer has the
// operand's type. Neither is a fault a test suite could have caught, so
// neither is generated.

func TestUnaryOperatorsAreNotMutatedAsBinaryOnes(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		fixture string
		// want is the mutator.Type the fixture's single operator must yield,
		// or the zero Type when the fixture must yield no mutant at all.
		want     mutator.Type
		mutating bool
	}{
		{
			name:     "a prefix & is address-of, not bitwise AND",
			fixture:  "testdata/fixtures/unary_and_go",
			mutating: false,
		},
		{
			name:     "a prefix ^ is complement, not XOR",
			fixture:  "testdata/fixtures/unary_xor_go",
			mutating: false,
		},
		{
			name:     "an infix & is still INVERT_BITWISE",
			fixture:  "testdata/fixtures/and_go",
			want:     mutator.InvertBitwise,
			mutating: true,
		},
		{
			name:     "an infix ^ is still INVERT_BITWISE",
			fixture:  "testdata/fixtures/xor_go",
			want:     mutator.InvertBitwise,
			mutating: true,
		},
		{
			name:     "a prefix - is still INVERT_NEGATIVES",
			fixture:  "testdata/fixtures/negative_sub_go",
			want:     mutator.InvertNegatives,
			mutating: true,
		},
		{
			name:     "an infix + is still ARITHMETIC_BASE",
			fixture:  "testdata/fixtures/add_go",
			want:     mutator.ArithmeticBase,
			mutating: true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			viperSet(map[string]any{configuration.UnleashDryRunKey: true})
			defer viperReset()

			mapFS, mod, c := loadFixture(tc.fixture, ".")
			defer c()

			mut := engine.New(mod, engine.CodeData{}, newJobDealerStub(t), engine.WithDirFs(mapFS))
			got := mut.Run(context.Background()).Mutants

			if !tc.mutating {
				if len(got) != 0 {
					t.Fatalf("expected no mutant, got %d: %s", len(got), describe(got))
				}

				return
			}

			for _, g := range got {
				if g.Type() == tc.want {
					return
				}
			}

			t.Fatalf("expected a %s mutant, got %s", tc.want, describe(got))
		})
	}
}

// operatorZoo exercises every Go operator gremlins has a mapping for, in both
// the prefix and the infix spelling wherever Go admits both. It is a whole
// compilable package so that the mutants generated from it can be handed to
// the real compiler.
const operatorZoo = `package zoo

type point struct{ x, y int }

func Address(i int) *int      { return &i }
func Composite() *point       { return &point{x: 1, y: 2} }
func Complement(u uint) uint  { return ^u }
func Negate(i int) int        { return -i }
func Identity(i int) int      { return +i }
func Receive(c chan int) int  { return <-c }
func Negation(b bool) bool    { return !b }
func And(a, b int) int        { return a & b }
func Or(a, b int) int         { return a | b }
func Xor(a, b int) int        { return a ^ b }
func AndNot(a, b int) int     { return a &^ b }
func Shl(a int) int           { return a << 1 }
func Shr(a int) int           { return a >> 1 }
func Add(a, b int) int        { return a + b }
func Sub(a, b int) int        { return a - b }
func Mul(a, b int) int        { return a * b }
func Quo(a, b int) int        { return a / b }
func Rem(a, b int) int        { return a % b }
func Greater(a, b int) bool   { return a > b }
func Equal(a, b int) bool     { return a == b }
func Both(a, b bool) bool     { return a && b }

func Assigns(a int) int {
	a += 1
	a -= 1
	a *= 2
	a /= 2
	a %= 2
	a &= 3
	a |= 3
	a ^= 3
	a &^= 3
	a <<= 1
	a >>= 1
	a++
	a--

	for {
		if a > 0 {
			break
		}

		continue
	}

	return a
}
`

const zooModule = "example.com"

// TestEveryGeneratedMutantCompiles is the gate on the whole mutation table,
// not just on the two prefix operators this change removes. A mutant the
// compiler rejects is not evidence about a test suite in either direction: it
// cannot be detected, so crediting a KILL for it pays the suite for the
// compiler's work, and recording it NOT VIABLE leaves a package's mutant set
// padded with entries that mean nothing. So the invariant is that gremlins
// does not emit one at all, and the only witness that settles it is the real
// compiler.
func TestEveryGeneratedMutantCompiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module "+zooModule+"\n\ngo 1.25\n")
	writeFile(t, dir, "zoo.go", operatorZoo)

	viperSet(map[string]any{configuration.UnleashDryRunKey: true})
	defer viperReset()

	mapFS := fstest.MapFS{"zoo.go": {Data: []byte(operatorZoo)}}
	mod := gomodule.GoModule{Name: zooModule, Root: ".", CallingDir: "."}
	mut := engine.New(mod, engine.CodeData{}, newJobDealerStub(t), engine.WithDirFs(mapFS))
	mutants := mut.Run(context.Background()).Mutants

	// A silent zero here would make every assertion below vacuous.
	if len(mutants) == 0 {
		t.Fatal("the operator zoo produced no mutants at all")
	}
	if out, err := goBuild(dir); err != nil {
		t.Fatalf("the unmutated operator zoo does not compile:\n%s", out)
	}

	for _, m := range mutants {
		m.SetWorkdir(dir)
		if err := m.Apply(); err != nil {
			t.Fatalf("applying the %s mutant at %s: %v", m.Type(), m.Position(), err)
		}
		out, buildErr := goBuild(dir)
		mutated := string(m.MutatedSnippet())
		if err := m.Rollback(); err != nil {
			t.Fatalf("rolling back the %s mutant at %s: %v", m.Type(), m.Position(), err)
		}

		if buildErr != nil {
			t.Errorf("%s at %s produced a mutant the compiler rejects:\n%s\nmutated source:\n%s",
				m.Type(), m.Position(), out, mutated)
		}
	}
}

// describe renders a mutant list as the type and position of each entry,
// which is what a failure here needs to name.
func describe(mutants []mutator.Mutator) string {
	described := make([]string, 0, len(mutants))
	for _, m := range mutants {
		described = append(described, fmt.Sprintf("%s at %s", m.Type(), m.Position()))
	}

	return strings.Join(described, ", ")
}

func goBuild(dir string) (string, error) {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()

	return string(out), err
}

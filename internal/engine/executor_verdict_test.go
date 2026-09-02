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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-gremlins/gremlins/internal/configuration"
	"github.com/go-gremlins/gremlins/internal/engine"
	"github.com/go-gremlins/gremlins/internal/engine/workerpool"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/internal/mutator"
)

// These tests drive the REAL `go` toolchain rather than a stubbed exec, because
// the thing under test is precisely how gremlins reads what `go test` reports.
// A stub could be made to say anything; only the real command proves that a
// timed-out mutant, a mutant that does not compile and a mutant that is killed
// are told apart. `go test` gives all three the same exit status 1, so a stub
// asserting exit codes would be asserting a fiction.

// baselineElapsed is the "how long the package's own tests take" figure fed to
// NewExecutorDealer, and runBoundCoefficient multiplies it. Together they put
// the coefficient-derived leash far above any ceiling a case sets, so each
// fixture's --timeout-max IS its run bound and no case depends on the
// coefficient arithmetic by accident.
const (
	baselineElapsed     = time.Second
	runBoundCoefficient = 3600
)

// generousCompileAllowance is large enough that no case here is adjudicated by
// the backstop except the one that means to be.
const generousCompileAllowance = "5m"

// noCompileAllowance is the smallest positive allowance the flag accepts. Paired
// with a short run bound it makes the backstop the binding constraint, which is
// how the compile-phase bound is exercised.
const noCompileAllowance = "1ms"

type goTestFixture struct {
	// sources maps a file name inside the throwaway module to its content.
	sources map[string]string
	// runBound is the value --timeout-max clamps the run leash to.
	runBound string
	// compileAllowance is the value of --compile-allowance.
	compileAllowance string
	// coldCache forces a private, empty build cache so compilation is
	// unambiguously slow. Without it the toolchain answers from cache and the
	// compile phase is too fast to say anything about.
	coldCache bool
}

// runMutant applies the fixture and returns the verdict the executor reached,
// along with how long the whole invocation took.
func runMutant(t *testing.T, fx goTestFixture) (mutator.Status, time.Duration) {
	t.Helper()

	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module gremlinsverdict\n\ngo 1.25\n")
	for name, content := range fx.sources {
		writeFile(t, dir, name, content)
	}

	if fx.coldCache {
		t.Setenv("GOCACHE", t.TempDir())
	}

	settings := map[string]any{
		configuration.UnleashDryRunKey:             false,
		configuration.UnleashTimeoutMaxKey:         fx.runBound,
		configuration.UnleashCompileAllowanceKey:   fx.compileAllowance,
		configuration.UnleashOnShutdownStatusKey:   "not-run",
		configuration.UnleashTimeoutCoefficientKey: runBoundCoefficient,
	}
	viperSet(settings)
	defer viperReset()

	wdDealer := &dealerStub{fnGet: func(string) (string, error) { return dir, nil }}
	mod := gomodule.GoModule{Name: "gremlinsverdict", Root: dir, CallingDir: "."}
	mjd := engine.NewExecutorDealer(mod, wdDealer, baselineElapsed)

	mut := &mutantStub{status: mutator.Runnable, mutType: mutator.ConditionalsBoundary, pkg: "."}
	outCh := make(chan mutator.Mutator, 1)
	wg := sync.WaitGroup{}
	wg.Add(1)
	executor := mjd.NewExecutor(mut, outCh, &wg)

	start := time.Now()
	executor.Start(&workerpool.Worker{Name: "test", ID: 1})
	wg.Wait()
	elapsed := time.Since(start)

	select {
	case got := <-outCh:
		return got.Status(), elapsed
	default:
		t.Fatal("executor never wrote a verdict")

		return mutator.NotCovered, elapsed
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// baseSource is the mutated code under test. Every fixture carries a non-test
// source file, because that is where a mutant lives.
const baseSource = `package verdict

func Answer() int { return 42 }
`

const passingTest = `package verdict

import "testing"

func TestPasses(t *testing.T) {
	if Answer() != 42 {
		t.Fatal("unexpected")
	}
}
`

const failingTest = `package verdict

import "testing"

func TestFails(t *testing.T) {
	if Answer() == 42 {
		t.Fatal("the mutant was detected")
	}
}
`

const nonTerminatingTest = `package verdict

import (
	"testing"
	"time"
)

func TestNeverTerminates(t *testing.T) {
	_ = Answer()
	time.Sleep(time.Hour)
}
`

const uncompilableSource = `package verdict

func Broken() int { return thisSymbolDoesNotExist() }
`

const uncompilableTest = `package verdict

import "testing"

func TestBroken(t *testing.T) { _ = Broken() }
`

// TestVerdictsFromRealGoTest pins each of the four outcomes independently.
// `go test` reports a failing test, a build failure and a test timeout all as
// exit status 1 — only the test BINARY exits 2, and what gremlins spawns is
// `go` — so any of the three could be mistaken for either of the others. Each
// direction is proven here rather than inferred from the others.
func TestVerdictsFromRealGoTest(t *testing.T) {
	testCases := []struct {
		name string
		fx   goTestFixture
		want mutator.Status
	}{
		{
			// The mutant survived: nothing about it made a test fail.
			name: "a passing test suite LIVES",
			fx: goTestFixture{
				sources:          map[string]string{"verdict.go": baseSource, "verdict_test.go": passingTest},
				runBound:         "60s",
				compileAllowance: generousCompileAllowance,
			},
			want: mutator.Lived,
		},
		{
			// A genuine detection, and the only outcome that may be credited.
			name: "a failing test KILLS",
			fx: goTestFixture{
				sources:          map[string]string{"verdict.go": baseSource, "verdict_test.go": failingTest},
				runBound:         "60s",
				compileAllowance: generousCompileAllowance,
			},
			want: mutator.Killed,
		},
		{
			// The case the timeout classification must not swallow. `go test`
			// exits 1 here, exactly as it does for a failing test, so reading
			// the exit status alone would credit a detection that never ran.
			name: "a mutant that does not compile is NOT VIABLE",
			fx: goTestFixture{
				sources: map[string]string{
					"verdict.go":      uncompilableSource,
					"verdict_test.go": uncompilableTest,
				},
				runBound:         "60s",
				compileAllowance: generousCompileAllowance,
			},
			want: mutator.NotViable,
		},
		{
			// The run-only leash firing. TIMED OUT stays in the efficacy
			// denominator and credits nobody; KILLED would credit a detection
			// that never happened, and NOT VIABLE would drop the mutant out of
			// the denominator altogether.
			name: "a test that does not terminate is TIMED OUT",
			fx: goTestFixture{
				sources:          map[string]string{"verdict.go": baseSource, "verdict_test.go": nonTerminatingTest},
				runBound:         "1s",
				compileAllowance: generousCompileAllowance,
			},
			want: mutator.TimedOut,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, _ := runMutant(t, tc.fx)
			if got != tc.want {
				t.Errorf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

// TestCompileIsNotChargedToTheRunBound is the fix itself, stated as behaviour.
//
// Both halves compile the same package with an empty build cache, so
// compilation demonstrably outlasts the run bound. With a compile allowance the
// mutant is adjudicated on its test, which passes in microseconds; without one
// the backstop fires. Before the split there was no allowance to give, and the
// first half was a TIMED OUT mutant that had never run a line of test code.
func TestCompileIsNotChargedToTheRunBound(t *testing.T) {
	const runBound = 2 * time.Second

	t.Run("a compile longer than the run bound still lets the test decide", func(t *testing.T) {
		got, elapsed := runMutant(t, goTestFixture{
			sources:          map[string]string{"verdict.go": baseSource, "verdict_test.go": passingTest},
			runBound:         runBound.String(),
			compileAllowance: generousCompileAllowance,
			coldCache:        true,
		})

		// Without this the case could pass vacuously on a machine where the
		// cold compile happened to fit inside the run bound, proving nothing.
		if elapsed <= runBound {
			t.Fatalf("cold compile took %s, which is inside the %s run bound: this case cannot"+
				" distinguish a run-only bound from a compile-and-run one", elapsed, runBound)
		}
		if got != mutator.Lived {
			t.Errorf("expected LIVED after a %s compile against a %s run bound, got %v", elapsed, runBound, got)
		}
	})

	t.Run("the backstop still bounds a compile that outlives it", func(t *testing.T) {
		// No -timeout can bound a compile: there is no test binary yet to
		// enforce one. The context deadline is the only thing that can, and it
		// must still report the honest verdict for an unadjudicated mutant.
		got, _ := runMutant(t, goTestFixture{
			sources:          map[string]string{"verdict.go": baseSource, "verdict_test.go": passingTest},
			runBound:         "100ms",
			compileAllowance: noCompileAllowance,
			coldCache:        true,
		})

		if got != mutator.TimedOut {
			t.Errorf("expected TIMED OUT from the compile backstop, got %v", got)
		}
	})
}

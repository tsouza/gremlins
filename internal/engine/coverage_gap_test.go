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
	"os"
	"strings"
	"testing"

	"github.com/tsouza/gremlins/internal/configuration"
	"github.com/tsouza/gremlins/internal/coverage"
	"github.com/tsouza/gremlins/internal/engine"
	"github.com/tsouza/gremlins/internal/engine/workdir"
	"github.com/tsouza/gremlins/internal/gomodule"
	"github.com/tsouza/gremlins/internal/mutator"
)

// gapSource carries the two shapes Go's cover tool leaves outside every block:
// a `case` expression, and a condition that follows a closure argument. Each
// line is tagged with what its mutants must be reported as.
//
//   - `// reached` lines are evaluated by the test below, so every mutant on
//     them must get a real verdict.
//   - `// unreached` lines sit in a function no test calls, or under a branch
//     no test takes, so their mutants must stay NOT COVERED. They are what
//     keeps the fix from reporting coverage the profile does not support.
const gapSource = `package gap

func call(f func() error) error { return f() }

func Classify(a, b int) int {
	switch {
	case a > b: // reached
		return 1
	case a < b: // reached
		return -1
	}
	if err := call(func() error {
		return nil
	}); err != nil { // reached
		return 2
	}
	if a == 999 {
		switch {
		case b > 3: // unreached
			return 3
		}
	}
	return 0
}

func Unreached(a, b int) int {
	switch {
	case a > b: // unreached
		return 1
	}
	if err := call(func() error {
		return nil
	}); err != nil { // unreached
		return 2
	}
	return 0
}
`

const gapTest = `package gap

import "testing"

func TestClassify(t *testing.T) {
	if Classify(2, 1) != 1 {
		t.Fatal("2 > 1")
	}
	if Classify(1, 2) != -1 {
		t.Fatal("1 < 2")
	}
	if Classify(1, 1) != 0 {
		t.Fatal("1 == 1")
	}
}
`

// gapRunBound is far above what gapTest takes; nothing here is timing-bound.
const gapRunBound = "1m"

// TestMutantsOutsideEveryCoverBlockAreRun drives the real coverage run, the
// real engine and the real executor over gapSource.
//
// Without the gap rule, every mutant on a `// reached` line is NOT COVERED:
// the position test asks whether the token lies inside an executed block, and
// none of these tokens lies inside any block. They are evaluated nonetheless,
// and the test kills most of them, so the only honest report is a verdict.
func TestMutantsOutsideEveryCoverBlockAreRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/gap\n\ngo 1.25\n")
	writeFile(t, dir, "gap.go", gapSource)
	writeFile(t, dir, "gap_test.go", gapTest)

	// coverage.Run changes directory into the module; put the test's back.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	viperSet(map[string]any{
		configuration.UnleashDryRunKey:           false,
		configuration.UnleashTimeoutMaxKey:       gapRunBound,
		configuration.UnleashCompileAllowanceKey: generousCompileAllowance,
	})
	defer viperReset()

	mod := gomodule.GoModule{Name: "example.com/gap", Root: dir, CallingDir: "."}
	workDir := t.TempDir()
	cov, err := coverage.New(workDir, mod).Run()
	if err != nil {
		t.Fatalf("coverage run: %v", err)
	}

	wdDealer := workdir.NewCachedDealer(workDir, dir)
	defer wdDealer.Clean()
	jDealer := engine.NewExecutorDealer(mod, wdDealer, baselineElapsed)
	eng := engine.New(mod, engine.CodeData{Cov: cov.Profile}, jDealer)
	res := eng.Run(context.Background())

	tags := lineTags(gapSource)
	seen := map[string]int{}
	killed := 0
	for _, m := range res.Mutants {
		tag := tags[m.Position().Line]
		if tag == "" {
			continue
		}
		seen[tag]++
		t.Logf("%-9s %s %s: %s", tag, m.Position(), m.Type(), m.Status())
		switch tag {
		case "reached":
			if m.Status() != mutator.Killed && m.Status() != mutator.Lived {
				t.Errorf("%s at %s on a line the test evaluates: got %s, want KILLED or LIVED",
					m.Type(), m.Position(), m.Status())
			}
			if m.Status() == mutator.Killed {
				killed++
			}
		case "unreached":
			if m.Status() != mutator.NotCovered {
				t.Errorf("%s at %s on a line no test evaluates: got %s, want NOT COVERED",
					m.Type(), m.Position(), m.Status())
			}
		}
	}

	// Without these the loop above could pass over an empty report.
	if seen["reached"] == 0 || seen["unreached"] == 0 {
		t.Fatalf("expected mutants on both kinds of line, got %v", seen)
	}
	if killed == 0 {
		t.Errorf("no mutant on a reached line was killed, so none was really run")
	}
}

// lineTags maps each 1-based line of src to the tag in its trailing comment.
func lineTags(src string) map[int]string {
	tags := map[int]string{}
	for i, line := range strings.Split(src, "\n") {
		if _, tag, ok := strings.Cut(line, "// "); ok {
			tags[i+1] = tag
		}
	}

	return tags
}

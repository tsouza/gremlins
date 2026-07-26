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

package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-gremlins/gremlins/internal/configuration"
	"github.com/go-gremlins/gremlins/internal/engine/workdir"
	"github.com/go-gremlins/gremlins/internal/engine/workerpool"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/internal/log"
	"github.com/go-gremlins/gremlins/internal/mutator"
)

// DefaultTimeoutCoefficient is the default multiplier for the timeout length
// of each test run.
const DefaultTimeoutCoefficient = 5

// noTimeoutMax is the timeout ceiling meaning "no ceiling": the per-mutant
// timeout is whatever the coefficient produces. This is the default, so the
// behaviour of a run that does not set --timeout-max is unchanged.
const noTimeoutMax = time.Duration(0)

// ExecutorDealer is the initializer for new workerpool.Executor.
type ExecutorDealer interface {
	NewExecutor(mut mutator.Mutator, outCh chan<- mutator.Mutator, wg *sync.WaitGroup) workerpool.Executor
}

// MutantExecutorDealer is a ExecutorDealer for the initialisation of a mutantExecutor.
//
// By default, it sets uses exec.Command to perform the tests on the source
// code. This can be overridden, for example in tests.
//
// The apply and rollback functions are wrappers around the TokenMutator apply and
// rollback. These can be overridden with nop functions in tests. Not an
// ideal setup. In the future we can think of a better way to handle this.
type MutantExecutorDealer struct {
	wdDealer          workdir.Dealer
	execContext       execContext
	runCtx            context.Context
	mod               gomodule.GoModule
	buildTags         string
	testExecutionTime time.Duration
	dryRun            bool
	integrationMode   bool
	testCPU           int
}

// SetRunCtx wires the engine's root context into the dealer so that each
// mutantExecutor it produces can observe cancellation (e.g. SIGTERM from
// a CI runner) and mark in-flight mutants accordingly instead of falling
// through to the default mutator.Lived branch.
func (m *MutantExecutorDealer) SetRunCtx(ctx context.Context) {
	m.runCtx = ctx
}

// ExecutorDealerOption is the defining option for the initialisation of a ExecutorDealer.
type ExecutorDealerOption func(j MutantExecutorDealer) MutantExecutorDealer

// WithExecContext overrides the default exec.Command with a custom executor.
func WithExecContext(c execContext) ExecutorDealerOption {
	return func(m MutantExecutorDealer) MutantExecutorDealer {
		m.execContext = c

		return m
	}
}

// NewExecutorDealer initialises a MutantExecutorDealer.
func NewExecutorDealer(mod gomodule.GoModule, wdd workdir.Dealer, elapsed time.Duration, opts ...ExecutorDealerOption) *MutantExecutorDealer {
	buildTags := configuration.Get[string](configuration.UnleashTagsKey)
	dryRun := configuration.Get[bool](configuration.UnleashDryRunKey)
	integrationMode := configuration.Get[bool](configuration.UnleashIntegrationMode)
	testCPU := configuration.Get[int](configuration.UnleashTestCPUKey)
	tCoefficient := configuration.Get[int](configuration.UnleashTimeoutCoefficientKey)

	coefficient := DefaultTimeoutCoefficient
	if tCoefficient != 0 {
		coefficient = tCoefficient
	}

	if testCPU != 0 && integrationMode {
		testCPU /= testCPU
	}

	// Use a minimum of 1 second for timeout calculation to prevent
	// unreasonably short timeouts when coverage runs very quickly
	baseTime := elapsed
	if baseTime < time.Second {
		baseTime = time.Second
	}

	jd := MutantExecutorDealer{
		mod:               mod,
		wdDealer:          wdd,
		buildTags:         buildTags,
		dryRun:            dryRun,
		integrationMode:   integrationMode,
		testCPU:           testCPU,
		testExecutionTime: cappedExecutionTime(baseTime * time.Duration(coefficient)),
		execContext:       exec.CommandContext,
	}

	for _, opt := range opts {
		jd = opt(jd)
	}

	return &jd
}

// cappedExecutionTime clamps a coefficient-derived per-mutant timeout to the
// absolute ceiling set by --timeout-max, if one is set.
//
// The coefficient-derived timeout is proportional to how long the package's own
// tests take, which is unrelated to how much damage a runaway mutant can do in
// that time. Mutating a loop-advance statement (i++ -> i--) inside a scanner
// loop whose body appends per iteration produces a mutant that never terminates
// and allocates until the machine is out of memory. On a CI runner the OOM
// killer then reaps the runner agent itself, so the job dies with no verdict at
// all — not a LIVED mutant, not a TIMED OUT one, just a dead runner. The
// timeout is the only defence against that, and scaling it by test duration
// hands the longest leash to the slowest-testing packages, which is backwards.
//
// A ceiling bounds the exposure independently of the baseline: past it the
// mutant is killed and recorded as TIMED OUT, which is the honest verdict for a
// mutant that did not terminate.
//
// Zero (the default) means no ceiling, so runs that do not set the flag behave
// exactly as before. A malformed or non-positive value is reported and ignored
// rather than silently treated as "no cap", because a safety ceiling that
// quietly does nothing is worse than one that was never configured.
func cappedExecutionTime(d time.Duration) time.Duration {
	raw := configuration.Get[string](configuration.UnleashTimeoutMaxKey)
	if raw == "" {
		return d
	}

	max, err := time.ParseDuration(raw)
	if err != nil {
		log.Errorf("invalid %s %q: %v; running without a timeout ceiling\n",
			configuration.UnleashTimeoutMaxKey, raw, err)

		return d
	}
	if max <= noTimeoutMax {
		log.Errorf("invalid %s %q: must be positive; running without a timeout ceiling\n",
			configuration.UnleashTimeoutMaxKey, raw)

		return d
	}

	if d > max {
		return max
	}

	return d
}

// NewExecutor returns a new workerpool.Executor for the given mutator.Mutator.
// It gets an output channel of mutator.Mutator and a sync.WaitGroup. The channel
// will stream the results of the executor, and the wait group will be done when the
// executor is complete.
func (m MutantExecutorDealer) NewExecutor(mut mutator.Mutator, outCh chan<- mutator.Mutator, wg *sync.WaitGroup) workerpool.Executor {
	runCtx := m.runCtx
	if runCtx == nil {
		runCtx = context.Background()
	}
	mj := mutantExecutor{
		mutant:            mut,
		outCh:             outCh,
		wg:                wg,
		wdDealer:          m.wdDealer,
		module:            m.mod,
		dryRun:            m.dryRun,
		integrationMode:   m.integrationMode,
		buildTags:         m.buildTags,
		execContext:       m.execContext,
		testCPU:           m.testCPU,
		testExecutionTime: m.testExecutionTime,
		runCtx:            runCtx,
	}

	return &mj
}

type execContext = func(ctx context.Context, name string, args ...string) *exec.Cmd

type mutantExecutor struct {
	mutant            mutator.Mutator
	wdDealer          workdir.Dealer
	outCh             chan<- mutator.Mutator
	wg                *sync.WaitGroup
	execContext       execContext
	runCtx            context.Context
	module            gomodule.GoModule
	buildTags         string
	testExecutionTime time.Duration
	dryRun            bool
	integrationMode   bool
	testCPU           int
}

// Start is the implementation of the workerpool.Executor definition and is the
// method responsible for performing the actual mutation testing.
// The executor runs on its mutator.Mutator.
// If it is RUNNABLE, and it is not in dry-run mode, it will apply the mutation,
// run the tests and mark the TokenMutator as either KILLED or LIVED depending
// on the result. If the tests pass, it means the TokenMutator survived, so it
// will be LIVED, if the tests fail, the TokenMutator will be KILLED.
// The timeout of the test is managed outside the run of the test, using
// a context with timeout. This is done because the Go test command doesn't
// make it easy to distinguish failures from timeouts.
func (m *mutantExecutor) Start(w *workerpool.Worker) {
	defer m.wg.Done()
	workerName := fmt.Sprintf("%s-%d", w.Name, w.ID)
	rootDir, err := m.wdDealer.Get(workerName)
	if err != nil {
		log.Errorf("failed to get working directory for worker %s: %v", workerName, err)
		panic(fmt.Sprintf("failed to get working directory for worker %s: %v", workerName, err))
	}

	workingDir := filepath.Join(rootDir, m.module.CallingDir)
	m.mutant.SetWorkdir(workingDir)

	if m.mutant.Status() == mutator.NotCovered || m.mutant.Status() == mutator.Skipped || m.dryRun {
		m.outCh <- m.mutant

		return
	}

	if err := m.mutant.Apply(); err != nil {
		log.Errorf("failed to apply mutation at %s - %s\n\t%v", m.mutant.Position(), m.mutant.Status(), err)

		return
	}

	m.mutant.SetStatus(m.runTests(rootDir, m.mutant.Pkg()))

	if err := m.mutant.Rollback(); err != nil {
		// What should we do now?
		log.Errorf("failed to restore mutation at %s - %s\n\t%v", m.mutant.Position(), m.mutant.Status(), err)
	}

	m.outCh <- m.mutant
}

func (m *mutantExecutor) runTests(rootDir, pkg string) mutator.Status {
	// Root the test ctx in the engine's runCtx so that an external
	// cancellation (e.g. SIGTERM from a CI runner) propagates into the
	// `go test` subprocess and we can distinguish "the runner is shutting
	// us down" from "the test ran past its deadline".
	ctx, cancel := context.WithTimeout(m.runCtx, m.testExecutionTime)
	defer cancel()

	cmd := m.execContext(ctx, "go", m.getTestArgs(pkg)...)
	cmd.Dir = m.mutant.Workdir()
	if m.integrationMode {
		cmd.Dir = rootDir
	}
	cmd.Env = append(cmd.Env, os.Environ()...)
	cmd.Env = append(cmd.Env, fmt.Sprintf("GOTMPDIR=%s", m.wdDealer.WorkDir()))

	// Set up process group for killing entire process tree
	setupProcessGroup(cmd)

	err := run(ctx, cmd)

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return mutator.TimedOut
	}
	// If the parent runCtx was cancelled (Ctrl-C or runner SIGTERM) the
	// `go test` child was killed before reaching a verdict. Reporting
	// these as LIVED misrepresents the data — they were never tested.
	// The configured shutdown status (default NotCovered) is the truthful
	// outcome.
	if m.runCtx.Err() != nil {
		return shutdownStatus()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return getTestFailedStatus(exitErr.ExitCode())
	}

	return mutator.Lived
}

// shutdownStatus returns the status to record for mutants that were
// in-flight when the run was cancelled. The choice is driven by the
// `unleash.on-shutdown-status` config key (CLI flag
// --on-shutdown-status). Unrecognised values fall back to NotCovered,
// the truthful default ("we never finished running this mutant").
func shutdownStatus() mutator.Status {
	v := configuration.Get[string](configuration.UnleashOnShutdownStatusKey)
	if s, ok := mutator.ParseShutdownStatus(v); ok {
		return s
	}
	return mutator.NotCovered
}

func (m *mutantExecutor) getTestArgs(pkg string) []string {
	args := []string{"test"}
	if m.buildTags != "" {
		args = append(args, "-tags", m.buildTags)
	}
	// Here we add some seconds to the timeout to be sure it's gremlins that catches the test
	// timeout and not the test itself. The timeout on the test prevents the test.* processes
	// from hanging forever.
	args = append(args, "-timeout", (2*time.Second + m.testExecutionTime).String())
	args = append(args, "-failfast")

	if m.testCPU != 0 {
		args = append(args, "-cpu", fmt.Sprintf("%d", m.testCPU))
	}

	path := pkg
	if m.integrationMode {
		path = "./..."
	}
	args = append(args, path)

	return args
}

func run(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}

	// Ensure cleanup happens regardless of how we exit
	defer func() {
		if cmd.Process != nil {
			// Always kill the process group to catch any child processes
			// This is safe even if the process already exited
			_ = killProcessGroup(cmd)
			// Release OS resources
			_ = cmd.Process.Release()
		}
	}()

	// Monitor context cancellation in parallel with process execution
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		// Context cancelled/timed out - kill the entire process group
		// Do this BEFORE the parent process exits to catch children
		_ = killProcessGroup(cmd)
		// Wait for the process to actually exit
		<-done

		return ctx.Err()
	case err := <-done:
		// Process completed normally
		return err
	}
}

func getTestFailedStatus(exitCode int) mutator.Status {
	switch exitCode {
	case 1:
		return mutator.Killed
	case 2:
		return mutator.NotViable
	default:
		return mutator.Lived
	}
}

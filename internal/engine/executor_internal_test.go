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
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestGetTestArgs(t *testing.T) {
	t.Parallel()

	testCases := map[string]struct {
		buildTags         string
		testExecutionTime time.Duration
		testCPU           int
		integrationMode   bool
		pkg               string
		want              []string
	}{
		"should_not_include_tags_flag_when_build_tags_are_empty": {
			testExecutionTime: 10 * time.Second,
			pkg:               "example.com/my/package",
			want:              []string{"test", "-timeout", "10s", "-failfast", "example.com/my/package"},
		},
		"should_include_tags_flag_when_build_tags_are_set": {
			buildTags:         "tag1,tag2",
			testExecutionTime: 10 * time.Second,
			pkg:               "example.com/my/package",
			want:              []string{"test", "-tags", "tag1,tag2", "-timeout", "10s", "-failfast", "example.com/my/package"},
		},
		"should_pass_the_run_bound_verbatim_so_compile_time_is_not_charged_to_it": {
			testExecutionTime: 30 * time.Second,
			pkg:               "example.com/my/package",
			want:              []string{"test", "-timeout", "30s", "-failfast", "example.com/my/package"},
		},
		"should_not_include_cpu_flag_when_test_cpu_is_zero": {
			testExecutionTime: 10 * time.Second,
			testCPU:           0,
			pkg:               "example.com/my/package",
			want:              []string{"test", "-timeout", "10s", "-failfast", "example.com/my/package"},
		},
		"should_include_cpu_flag_when_test_cpu_is_nonzero": {
			testExecutionTime: 10 * time.Second,
			testCPU:           4,
			pkg:               "example.com/my/package",
			want:              []string{"test", "-timeout", "10s", "-failfast", "-cpu", "4", "example.com/my/package"},
		},
		"should_use_package_path_when_integration_mode_is_disabled": {
			testExecutionTime: 10 * time.Second,
			integrationMode:   false,
			pkg:               "example.com/my/package",
			want:              []string{"test", "-timeout", "10s", "-failfast", "example.com/my/package"},
		},
		"should_use_dot_dot_dot_path_when_integration_mode_is_enabled": {
			testExecutionTime: 10 * time.Second,
			integrationMode:   true,
			pkg:               "example.com/my/package",
			want:              []string{"test", "-timeout", "10s", "-failfast", "./..."},
		},
		"should_include_all_flags_when_all_options_are_configured": {
			buildTags:         "integration",
			testExecutionTime: 10 * time.Second,
			testCPU:           2,
			integrationMode:   true,
			pkg:               "example.com/my/package",
			want:              []string{"test", "-tags", "integration", "-timeout", "10s", "-failfast", "-cpu", "2", "./..."},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			sut := &mutantExecutor{
				buildTags:         tc.buildTags,
				testExecutionTime: tc.testExecutionTime,
				testCPU:           tc.testCPU,
				integrationMode:   tc.integrationMode,
			}

			if diff := cmp.Diff(tc.want, sut.getTestArgs(tc.pkg)); diff != "" {
				t.Errorf("getTestArgs() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestOutputScanner pins the two properties the verdict classification depends
// on: a marker is recognised however the child's output happens to be chunked,
// and the scanner never grows with the volume it is fed.
func TestOutputScanner(t *testing.T) {
	t.Parallel()

	t.Run("recognises a marker split across writes", func(t *testing.T) {
		t.Parallel()

		// The pipe between `go test` and gremlins chunks at whatever boundary
		// the kernel chooses, so the marker is split at every offset in turn
		// rather than at one arbitrary place.
		for i := 1; i < len(testTimeoutMarker); i++ {
			sut := newOutputScanner()
			mustWrite(t, sut, "some earlier output\n"+testTimeoutMarker[:i])
			mustWrite(t, sut, testTimeoutMarker[i:]+"10s\n\ngoroutine 1 [running]:\n")

			if !sut.sawTestTimeout() {
				t.Fatalf("marker split after %d bytes went unnoticed", i)
			}
		}
	})

	t.Run("the other exit-1 outcomes trip nothing", func(t *testing.T) {
		t.Parallel()

		// Both of these leave `go test`'s exit status 1 to speak for itself. A
		// failing test is a detection and belongs there; a build failure is not,
		// and is the subject of cerberus issue #2930.
		for name, output := range map[string]string{
			"a failing test": "--- FAIL: TestX (0.00s)\n    x_test.go:9: boom\n" +
				"FAIL\texample.com/pkg\t0.005s\n",
			"a build failure": "# example.com/pkg [example.com/pkg.test]\n" +
				"./x.go:5:2: undefined: missing\nFAIL\texample.com/pkg [build failed]\n",
		} {
			sut := newOutputScanner()
			mustWrite(t, sut, output)

			if sut.sawTestTimeout() {
				t.Errorf("%s was read as a timeout", name)
			}
		}
	})

	t.Run("retains a constant amount however much it is fed", func(t *testing.T) {
		t.Parallel()

		// A runaway mutant can print without bound; buffering that output would
		// trade the resource exhaustion this fork exists to prevent for another.
		sut := newOutputScanner()
		const chunk = 64 * 1024
		const chunks = 64
		for i := 0; i < chunks; i++ {
			mustWrite(t, sut, strings.Repeat("x", chunk))
		}

		if len(sut.tail) > sut.carry {
			t.Errorf("retained %d bytes after %d, want at most %d", len(sut.tail), chunk*chunks, sut.carry)
		}
	})
}

func mustWrite(t *testing.T, sut *outputScanner, s string) {
	t.Helper()

	n, err := sut.Write([]byte(s))
	if err != nil || n != len(s) {
		t.Fatalf("Write(%d bytes) = %d, %v", len(s), n, err)
	}
}

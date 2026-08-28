// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package execx

import (
	"context"
	"testing"
	"time"
)

func collect(t *testing.T, ch <-chan Event) (lines []string, final Event) {
	t.Helper()
	for ev := range ch {
		if ev.Done {
			return lines, ev
		}
		lines = append(lines, ev.Line)
	}
	t.Fatal("channel closed without a Done event")
	return nil, Event{}
}

func TestRealStreamsOutputAndExit(t *testing.T) {
	ch := Real{}.Start(context.Background(), Spec{
		Argv: []string{"sh", "-c", `printf 'one\n\x1b[1;36mtwo\x1b[0m\n'; echo err >&2`},
	})
	lines, final := collect(t, ch)
	if final.Err != nil {
		t.Fatalf("unexpected error: %v", final.Err)
	}
	got := map[string]bool{}
	for _, l := range lines {
		got[l] = true
	}
	// ANSI codes must be stripped; stderr must be interleaved.
	for _, want := range []string{"one", "two", "err"} {
		if !got[want] {
			t.Errorf("missing line %q in %v", want, lines)
		}
	}
}

func TestRealReportsFailure(t *testing.T) {
	ch := Real{}.Start(context.Background(), Spec{Argv: []string{"sh", "-c", "exit 3"}})
	_, final := collect(t, ch)
	if final.Err == nil {
		t.Fatal("want an error for exit 3")
	}
}

func TestRealAppliesEnvAndDir(t *testing.T) {
	dir := t.TempDir()
	ch := Real{}.Start(context.Background(), Spec{
		Argv: []string{"sh", "-c", "echo $FOO; pwd"},
		Env:  []string{"FOO=bar"},
		Dir:  dir,
	})
	lines, final := collect(t, ch)
	if final.Err != nil {
		t.Fatal(final.Err)
	}
	if len(lines) != 2 || lines[0] != "bar" {
		t.Fatalf("lines = %v", lines)
	}
}

func TestDryRunReplaysSimLines(t *testing.T) {
	spec := Spec{Display: "go run ./x", SimLines: []string{"a", "b"}}
	ch := DryRun{Delay: time.Millisecond}.Start(context.Background(), spec)
	lines, final := collect(t, ch)
	if final.Err != nil {
		t.Fatal(final.Err)
	}
	if len(lines) != 3 || lines[1] != "a" || lines[2] != "b" {
		t.Fatalf("lines = %v", lines)
	}
}

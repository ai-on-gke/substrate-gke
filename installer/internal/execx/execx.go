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

// Package execx streams the output of external commands to the TUI. This is
// the seam the onboarding prototype lacked: the same screens run either the
// real installer commands or, under --dry-run, a scripted replay.
package execx

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Spec describes one external command.
type Spec struct {
	// Label is a short name for progress display.
	Label string
	// Display is the human-readable command line shown to the user.
	Display string
	// Dir is the working directory.
	Dir string
	// Argv is the command and its arguments.
	Argv []string
	// Env holds extra KEY=VALUE entries layered over the process environment.
	Env []string
	// SimLines is what a --dry-run replays instead of executing Argv.
	SimLines []string
}

// Event is one unit of command progress.
type Event struct {
	// Line is one line of combined stdout/stderr output, ANSI-stripped.
	Line string
	// Done marks the final event; Err is the command error, if any.
	Done bool
	Err  error
	// Stderr indicates this line originated from standard error.
	Stderr bool
}

// Runner starts commands and streams their output.
type Runner interface {
	Start(ctx context.Context, spec Spec) <-chan Event
}

var (
	ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	// OSC sequences (terminal titles, hyperlinks) pass the CSI-only pattern
	// and would reprogram the terminal if re-emitted by the log viewer.
	osc = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)?`)
)

const maxLineLen = 2000

// Clean strips ANSI escapes, resolves in-line carriage returns, and
// truncates pathological lines.
func Clean(line string) string {
	line = strings.TrimRight(line, "\r\n")
	// Progress redraws pack "10%\r50%\r100%" into one scanner line; a
	// terminal would show only the last segment, so keep only that —
	// re-emitting the \r would overwrite whatever panel row it lands on.
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	line = ansi.ReplaceAllString(line, "")
	line = osc.ReplaceAllString(line, "")
	if len(line) > maxLineLen {
		line = line[:maxLineLen] + "…"
	}
	return line
}

func clean(line string) string { return Clean(line) }

// Real executes commands with os/exec.
type Real struct {
	Log *Logger

	mu       sync.Mutex
	lastDone chan struct{}
	wg       sync.WaitGroup
}

// Drain waits for in-flight commands to finish draining pipes and writing logs.
func (r *Real) Drain() {
	if r == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// Start runs the command and streams its combined output line by line. The
// channel is closed after the Done event.
func (r *Real) Start(ctx context.Context, spec Spec) <-chan Event {
	ch := make(chan Event, 64)

	var prev <-chan struct{}
	var done chan struct{}
	if r != nil {
		r.mu.Lock()
		prev = r.lastDone
		done = make(chan struct{})
		r.lastDone = done
		r.wg.Add(1)
		r.mu.Unlock()
	}

	go func() {
		defer close(ch)
		if done != nil {
			defer close(done)
			defer r.wg.Done()
		}

		// Wait for any previous command to finish draining before logging start.
		if prev != nil {
			select {
			case <-prev:
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				ch <- Event{Done: true, Err: ctx.Err()}
				return
			}
		}

		start := time.Now()
		if r != nil && r.Log != nil {
			r.Log.LogCommandStart(spec)
		}

		cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
		cmd.Dir = spec.Dir
		cmd.Env = append(os.Environ(), spec.Env...)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			if r != nil && r.Log != nil {
				r.Log.LogCommandEnd(spec, err, time.Since(start))
			}
			ch <- Event{Done: true, Err: err}
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			if r != nil && r.Log != nil {
				r.Log.LogCommandEnd(spec, err, time.Since(start))
			}
			ch <- Event{Done: true, Err: err}
			return
		}
		if err := cmd.Start(); err != nil {
			if r != nil && r.Log != nil {
				r.Log.LogCommandEnd(spec, err, time.Since(start))
			}
			ch <- Event{Done: true, Err: err}
			return
		}

		var wg sync.WaitGroup
		scan := func(rd io.Reader, isStderr bool) {
			defer wg.Done()
			sc := bufio.NewScanner(rd)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			// Once the UI abandons the channel (cancel stops its reader), a
			// blocking send would wedge this goroutine forever: the log
			// would lose its tail and END record, Drain would always time
			// out, and the next command's log section would interleave with
			// this one's. Keep logging to disk; just stop feeding the
			// channel.
			abandoned := false
			for sc.Scan() {
				raw := sc.Text()
				if r != nil && r.Log != nil {
					r.Log.LogLine(raw)
				}
				if abandoned {
					continue
				}
				if line := Clean(raw); line != "" {
					select {
					case ch <- Event{Line: line, Stderr: isStderr}:
					case <-ctx.Done():
						abandoned = true
					}
				}
			}
		}
		wg.Add(2)
		go scan(stdout, false)
		go scan(stderr, true)
		wg.Wait()

		waitErr := cmd.Wait()
		if r != nil && r.Log != nil {
			r.Log.LogCommandEnd(spec, waitErr, time.Since(start))
		}
		select {
		case ch <- Event{Done: true, Err: waitErr}:
		case <-ctx.Done():
		}
	}()
	return ch
}

// DryRun replays a Spec's SimLines with a small delay instead of executing.
type DryRun struct {
	// Delay between replayed lines; defaults to 250ms.
	Delay time.Duration
	Log   *Logger
}

// Start replays spec.SimLines and finishes successfully.
func (d DryRun) Start(ctx context.Context, spec Spec) <-chan Event {
	delay := d.Delay
	if delay == 0 {
		delay = 250 * time.Millisecond
	}
	ch := make(chan Event, 64)
	go func() {
		defer close(ch)
		start := time.Now()
		if d.Log != nil {
			d.Log.LogCommandStart(spec)
		}
		ch <- Event{Line: "(dry-run) " + spec.Display}
		if d.Log != nil {
			d.Log.LogLine("(dry-run) " + spec.Display)
		}
		for _, line := range spec.SimLines {
			select {
			case <-ctx.Done():
				if d.Log != nil {
					d.Log.LogCommandEnd(spec, ctx.Err(), time.Since(start))
				}
				ch <- Event{Done: true, Err: ctx.Err()}
				return
			case <-time.After(delay):
			}
			if d.Log != nil {
				d.Log.LogLine(line)
			}
			ch <- Event{Line: line}
		}
		if d.Log != nil {
			d.Log.LogCommandEnd(spec, nil, time.Since(start))
		}
		ch <- Event{Done: true}
	}()
	return ch
}

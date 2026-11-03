// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runRoot executes rootCmd with args, with a throwaway subcommand whose RunE
// fails with runErr, and returns what was printed and the error.
func runRoot(t *testing.T, runErr error, args ...string) (string, error) {
	t.Helper()
	sub := &cobra.Command{
		Use:  "fail-for-test",
		RunE: func(*cobra.Command, []string) error { return runErr },
	}
	rootCmd.AddCommand(sub)
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.RemoveCommand(sub)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err := rootCmd.Execute()
	return out.String(), err
}

// ai-on-gke/substrate-gke#115: an error from a command's RunE is returned
// for main to print, and nothing else is printed, in particular not the usage
// text that buried the error on the installer's screen.
func TestExecuteKeepsUsageOutOfRuntimeErrors(t *testing.T) {
	runErr := errors.New("update dashboard: rpc error: code = DeadlineExceeded")
	out, err := runRoot(t, runErr, "fail-for-test")
	if !errors.Is(err, runErr) {
		t.Fatalf("Execute() = %v, want %v", err, runErr)
	}
	if out != "" {
		t.Errorf("Execute() printed:\n%s\nwant nothing: main prints the error", out)
	}
}

// A flag the command does not know, and --help, still show the usage text.
func TestExecuteShowsUsageForFlagErrorsAndHelp(t *testing.T) {
	out, err := runRoot(t, nil, "fail-for-test", "--no-such-flag")
	if err == nil || !strings.Contains(err.Error(), "no-such-flag") {
		t.Fatalf("Execute() = %v, want an unknown flag error", err)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("Execute() printed:\n%s\nwant the usage text for a bad flag", out)
	}

	out, err = runRoot(t, nil, "fail-for-test", "--help")
	if err != nil {
		t.Fatalf("Execute() = %v, want --help to succeed", err)
	}
	if !strings.Contains(out, "Usage:") || !strings.Contains(out, "--project-id") {
		t.Errorf("Execute() printed:\n%s\nwant the help text with the persistent flags", out)
	}
}

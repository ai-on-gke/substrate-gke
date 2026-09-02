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

//go:build darwin || linux

package snapshot

import (
	"os"
	"syscall"
)

// sharedLock opens path (creating it if need be) and takes a shared advisory
// lock on it. Blocking is fine here: the only exclusive holders are cleanups,
// which finish in moments. The lock lives as long as the returned file stays
// open.
func sharedLock(path string) (*os.File, error) {
	return flock(path, syscall.LOCK_SH)
}

// exclusiveLock tries a non-blocking exclusive lock on path. Failure means a
// live run holds the lock shared — the signal Cleanup keys off.
func exclusiveLock(path string) (*os.File, error) {
	return flock(path, syscall.LOCK_EX|syscall.LOCK_NB)
}

func flock(path string, how int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

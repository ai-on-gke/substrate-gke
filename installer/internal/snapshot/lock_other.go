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

//go:build !(darwin || linux)

package snapshot

import "os"

// Without flock the locks degrade to no-ops: runs are unprotected from each
// other's cleanups, exactly as they were before locking existed. The nil
// files tell callers there is nothing to hold or release.

func sharedLock(string) (*os.File, error)    { return nil, nil }
func exclusiveLock(string) (*os.File, error) { return nil, nil }

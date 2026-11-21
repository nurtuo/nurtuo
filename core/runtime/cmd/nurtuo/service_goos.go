// SPDX-License-Identifier: AGPL-3.0-only

package main

import goruntime "runtime"

func goos() string { return goruntime.GOOS }

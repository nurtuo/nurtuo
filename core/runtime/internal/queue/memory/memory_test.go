// SPDX-License-Identifier: AGPL-3.0-only

package memory_test

import (
	"testing"

	"github.com/nurtuo/nurtuo/core/runtime/internal/queue"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/memory"
	"github.com/nurtuo/nurtuo/core/runtime/internal/queue/queuetest"
)

func TestConformance(t *testing.T) {
	queuetest.Run(t, func(t *testing.T) queue.QueueBackend { return memory.New() })
}

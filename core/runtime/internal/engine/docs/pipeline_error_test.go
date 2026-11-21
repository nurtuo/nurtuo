// SPDX-License-Identifier: AGPL-3.0-only

package docs

import (
	"context"
	"fmt"
	"testing"
)

func TestRunOneDoesNotMutateSharedError(t *testing.T) {
	shared := &Error{Message: "boom"}
	var in []*Document
	for i := 0; i < 4; i++ {
		in = append(in, New(fmt.Sprint(i), nil, nil))
	}
	out, _ := Apply(context.Background(), NewSet(in...), Options{Step: "s1"}, func(ctx context.Context, d *Document) (*Document, error) {
		return nil, shared
	})
	if shared.Step != "" {
		t.Fatalf("shared error mutated: Step=%q", shared.Step)
	}
	_ = out
}

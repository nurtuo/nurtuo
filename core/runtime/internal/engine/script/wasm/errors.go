// SPDX-License-Identifier: AGPL-3.0-only

package wasm

import (
	"context"
	"errors"
	"strings"

	"github.com/tetratelabs/wazero/sys"
)

// Error codes. A failed invocation always yields an *Error carrying one of these, so the engine
// can fail exactly one document with a stable, documented reason.
const (
	CodeTimeout          = "timeout"           // deadline exceeded (also: caller context done)
	CodeTrap             = "trap"              // guest trapped (unreachable, out-of-bounds, div by zero, stack overflow, ...)
	CodeMemoryLimit      = "memory_limit"      // memory cap hit: guest could not allocate, or declared minimum above the cap
	CodeCapabilityDenied = "capability_denied" // guest used a capability it was not granted
	CodeScript           = "script_error"      // guest reported a failure through nurtuo.error
	CodeExit             = "guest_exit"        // guest called proc_exit
	CodeInput            = "input"             // input could not be encoded or is above the limit
	CodeOutput           = "output_invalid"    // returned output is out of range or malformed
	CodeOutputTooLarge   = "output_too_large"
	CodeInvalidModule    = "invalid_module" // not a valid module, or imports something the sandbox does not provide
	CodeABI              = "abi"            // valid module that does not implement the Nurtuo ABI
	CodeInternal         = "internal"
)

// Error is the error type returned by Engine.Compile and Module.Run.
type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string { return "script " + e.Code + ": " + e.Message }

// Unwrap returns the underlying cause (for example a wazero error), if any.
func (e *Error) Unwrap() error { return e.Cause }

func classify(ctx context.Context, st *invocation, phase string, err error) *Error {
	var ee *sys.ExitError
	switch {
	case st.denied != "":
		return &Error{Code: CodeCapabilityDenied, Message: "guest used capability " + st.denied + " which was not granted", Cause: err}
	case errors.As(err, &ee) && ee.ExitCode() == sys.ExitCodeDeadlineExceeded,
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &Error{Code: CodeTimeout, Message: "execution time limit exceeded during " + phase, Cause: err}
	case errors.As(err, &ee) && ee.ExitCode() == sys.ExitCodeContextCanceled, errors.Is(err, context.Canceled):
		return &Error{Code: CodeTimeout, Message: "execution cancelled during " + phase, Cause: err}
	case st.failure != "":
		return &Error{Code: CodeScript, Message: st.failure, Cause: err}
	case errors.As(err, &ee):
		return &Error{Code: CodeExit, Message: err.Error(), Cause: err}
	case phase == "instantiate" && strings.Contains(err.Error(), "memory"):
		return &Error{Code: CodeMemoryLimit, Message: "instantiate: " + err.Error(), Cause: err}
	default:
		return &Error{Code: CodeTrap, Message: phase + ": " + firstLine(err.Error()), Cause: err}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

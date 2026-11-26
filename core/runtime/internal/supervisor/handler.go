// SPDX-License-Identifier: AGPL-3.0-only

package supervisor

import (
	"context"

	gatewayv1 "github.com/nurtuo/nurtuo/sdk/go/gen/nurtuo/runtime/v1"
)

// CommandFunc executes one named control-plane command.
type CommandFunc func(ctx context.Context, args map[string]string) error

// Handler adapts a Supervisor to gateway.Handler.
type Handler struct {
	S        *Supervisor
	Commands map[string]CommandFunc // pause_listener, resume_listener, ... registered by the engine
	// Report, when set, receives deployment status changes caused by commands (running <-> paused).
	Report func(*gatewayv1.DeploymentStatus)
}

func (h *Handler) Revision() uint64 { return h.S.liveRevision() }

func (h *Handler) ApplyDesiredState(ctx context.Context, ds *gatewayv1.DesiredState, report func(*gatewayv1.DeploymentStatus)) error {
	var want []Deployment
	for _, d := range ds.GetDeployments() {
		want = append(want, Deployment{ID: d.DeploymentId, BundleDigest: d.BundleDigest, QueueIDs: d.QueueIds, ListenerIDs: d.ListenerIds})
	}
	return h.S.Reconcile(ctx, ds.GetRevision(), want, func(s Status) {
		report(&gatewayv1.DeploymentStatus{Revision: s.Revision, DeploymentId: s.DeploymentID, State: s.State, Message: s.Message})
	})
}

func (h *Handler) HandleCommand(ctx context.Context, cmd *gatewayv1.Command) *gatewayv1.CommandResult {
	fn, ok := h.Commands[cmd.GetName()]
	if !ok {
		return &gatewayv1.CommandResult{CommandId: cmd.GetCommandId(), Ok: false, Message: "unknown command: " + cmd.GetName()}
	}
	if err := fn(ctx, cmd.GetArgs()); err != nil {
		return &gatewayv1.CommandResult{CommandId: cmd.GetCommandId(), Ok: false, Message: err.Error()}
	}
	if h.Report != nil {
		h.S.ReportChanges(ctx, func(s Status) {
			h.Report(&gatewayv1.DeploymentStatus{Revision: s.Revision, DeploymentId: s.DeploymentID, State: s.State, Message: s.Message})
		})
	}
	return &gatewayv1.CommandResult{CommandId: cmd.GetCommandId(), Ok: true}
}

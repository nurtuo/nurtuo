// SPDX-License-Identifier: AGPL-3.0-only

package supervisor

import "context"

// Deployment is the runtime-local view of one desired deployment.
type Deployment struct {
	ID           string
	BundleDigest string
	QueueIDs     []string
	ListenerIDs  []string
}

// Deployer is the engine-facing seam. The real engine implements it later; tests use a fake.
// Implementations must be idempotent: Apply on an existing ID updates it in place.
type Deployer interface {
	// Actual returns the deployments currently running, keyed by deployment ID.
	Actual(ctx context.Context) (map[string]Deployment, error)
	// Apply starts a new deployment or updates an existing one.
	Apply(ctx context.Context, d Deployment) error
	// Remove stops and removes a deployment.
	Remove(ctx context.Context, id string) error
}

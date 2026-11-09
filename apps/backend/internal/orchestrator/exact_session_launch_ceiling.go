package orchestrator

import (
	"context"
	"fmt"
)

// errExactLaunchCeilingRefused reports that the workspace ceiling declined an
// exact session launch. The exact path never defers: a deferred replay would
// launch later with no operation owning it.
var errExactLaunchCeilingRefused = fmt.Errorf("orchestrator: session ceiling refused the exact launch")

type exactLaunchCeilingRefusalKey struct{}

// withExactLaunchCeilingRefusal marks a launch as an exact session launch:
// it is admitted as an automatic launch (a manual origin could override the
// ceiling) and a refusal is returned instead of being queued as a deferral.
func withExactLaunchCeilingRefusal(ctx context.Context) context.Context {
	return context.WithValue(ctx, exactLaunchCeilingRefusalKey{}, true)
}

func exactLaunchCeilingRefusal(ctx context.Context) bool {
	v, _ := ctx.Value(exactLaunchCeilingRefusalKey{}).(bool)
	return v
}

// launchOriginForStart is the origin a created-session start is admitted
// under. An exact launch is always automatic so it cannot override the ceiling.
func launchOriginForStart(ctx context.Context, autoStart bool) launchOrigin {
	if exactLaunchCeilingRefusal(ctx) {
		return launchOriginAutomatic
	}
	return originFromAutoStart(autoStart)
}

func exactLaunchCeilingRefusedError(reasonCode string) error {
	return fmt.Errorf("%w: %s", errExactLaunchCeilingRefused, reasonCode)
}

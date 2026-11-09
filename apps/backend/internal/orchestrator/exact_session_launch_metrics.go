package orchestrator

import "expvar"

// expvar maps for exact fresh-session launches. Labels are closed sets; no
// task, session, key, or operation identifier is ever a label.
var (
	exactSessionLaunchOperationsTotal = expvar.NewMap("exact_session_launch_operations_total")
	exactSessionLaunchReconcileTotal  = expvar.NewMap("exact_session_launch_reconcile_total")
	exactSessionLaunchPrimaryTotal    = expvar.NewMap("exact_session_launch_primary_total")
)

const (
	exactLaunchReconcileResumed  = "resumed"
	exactLaunchReconcileOwned    = "skipped_owned"
	exactLaunchReconcileListFail = "list_failed"
	exactLaunchReconcileDriveErr = "drive_error"
)

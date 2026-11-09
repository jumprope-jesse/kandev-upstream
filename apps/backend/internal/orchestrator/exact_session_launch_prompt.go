package orchestrator

import (
	"errors"
	"sync"
)

type exactPromptOutcome int

const (
	exactPromptOpen exactPromptOutcome = iota
	exactPromptAccepted
	exactPromptUncertain
	exactPromptNotDelivered
)

const (
	exactCodeAdmissionRejected = "admission_rejected"
	exactCodeDispatchUncertain = "dispatch_uncertain"
	exactCodeLaunchFailed      = "launch_failed"
	exactCodeLaunchTimeout     = "launch_timeout"
	exactCodeSessionBusy       = "session_busy"
	exactCodeCeilingRefused    = "ceiling_refused"
	exactCodePrepareFailed     = "prepare_failed"
	exactCodeNotStartable      = "session_not_startable"
	exactCodeSubmissionLost    = "submission_unavailable"
	exactCodeShutdown          = "shutdown"
)

var errExactPromptGateClosed = errors.New("exact session launch: prompt gate closed")

// exactPromptSignal settles an initial prompt exactly once. The launch
// callbacks fire from a detached sender, so the first terminal signal wins and
// a failure reported after acceptance is ignored. Once dispatching is set the
// prompt may have reached the provider, so a later failure is uncertain, never
// not-delivered, unless the provider admission itself refused it.
type exactPromptSignal struct {
	mu          sync.Mutex
	dispatching bool
	rejected    bool
	closed      bool
	outcome     exactPromptOutcome
	code        string
	done        chan struct{}
}

func newExactPromptSignal() *exactPromptSignal {
	return &exactPromptSignal{done: make(chan struct{})}
}

// enterDispatch marks the point of no return and refuses once the gate closed.
func (g *exactPromptSignal) enterDispatch() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.outcome != exactPromptOpen {
		return errExactPromptGateClosed
	}
	g.dispatching = true
	return nil
}

func (g *exactPromptSignal) rejectAdmission() {
	g.mu.Lock()
	g.rejected = true
	g.mu.Unlock()
}

func (g *exactPromptSignal) isDispatching() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dispatching
}

func (g *exactPromptSignal) settleLocked(outcome exactPromptOutcome, code string) {
	if g.outcome != exactPromptOpen {
		return
	}
	g.outcome, g.code = outcome, code
	close(g.done)
}

func (g *exactPromptSignal) accepted() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settleLocked(exactPromptAccepted, "")
}

func (g *exactPromptSignal) failedLocked(code string) {
	switch {
	case g.rejected:
		g.settleLocked(exactPromptNotDelivered, exactCodeAdmissionRejected)
	case g.dispatching:
		g.settleLocked(exactPromptUncertain, exactCodeDispatchUncertain)
	default:
		g.settleLocked(exactPromptNotDelivered, code)
	}
}

func (g *exactPromptSignal) failed(code string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failedLocked(code)
}

// closeGate refuses any later dispatch and returns the current outcome. When
// settleUndispatched is false an operation that never reached dispatch stays
// open, so a restart can resume it.
func (g *exactPromptSignal) closeGate(code string, settleUndispatched bool) (exactPromptOutcome, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	if g.outcome == exactPromptOpen && (g.dispatching || settleUndispatched) {
		g.failedLocked(code)
	}
	return g.outcome, g.code
}

func (g *exactPromptSignal) result() (exactPromptOutcome, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.outcome, g.code
}

package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/kandev/kandev/internal/agentruntime"
	"github.com/kandev/kandev/internal/task/models"
	"github.com/stretchr/testify/require"
)

func queueGuardedRemote(t *testing.T) (*Manager, *models.ExecutorRunning, *remoteRecoveryRetry) {
	t.Helper()
	manager, _ := task08ManagerWithBackend(t, NewSSHExecutor(nil, nil, nil, newTestLogger()))
	row := &models.ExecutorRunning{
		SessionID: "session-guard-life", AgentExecutionID: "execution-guard-life",
		Runtime: agentruntime.RuntimeSSH,
	}
	entry := manager.queueRemoteRecovery(row, nil)
	require.NotNil(t, entry)
	require.ErrorIs(t, manager.recoveryGuard.CheckLaunchAllowed(row.SessionID), ErrSessionRecoveryGuarded)
	return manager, row, entry
}

func TestRemoteRecoveryGuardExpiresAfterMaxAge(t *testing.T) {
	manager, row, entry := queueGuardedRemote(t)

	manager.retryRemoteRecovery(context.Background(), entry)
	require.ErrorIs(t, manager.recoveryGuard.CheckLaunchAllowed(row.SessionID), ErrSessionRecoveryGuarded,
		"a young guard stays held")

	manager.remoteRecoveryMu.Lock()
	entry.queuedAt = time.Now().Add(-remoteRecoveryGuardMaxAge - time.Second)
	manager.remoteRecoveryMu.Unlock()
	manager.retryRemoteRecovery(context.Background(), entry)

	require.NoError(t, manager.recoveryGuard.CheckLaunchAllowed(row.SessionID))
	require.False(t, manager.HasPendingRemoteRecovery(row.SessionID, row.AgentExecutionID))
}

func TestRemoteRecoveryGuardKeptWhileStopPending(t *testing.T) {
	manager, row, entry := queueGuardedRemote(t)
	manager.remoteRecoveryMu.Lock()
	entry.stopRequested = true
	entry.queuedAt = time.Now().Add(-2 * remoteRecoveryGuardMaxAge)
	manager.remoteRecoveryMu.Unlock()

	require.False(t, manager.expireRemoteRecoveryGuard(entry))
	require.ErrorIs(t, manager.recoveryGuard.CheckLaunchAllowed(row.SessionID), ErrSessionRecoveryGuarded)
}

func TestReleaseRemoteRecoveryGuardIsExplicitOperatorRelease(t *testing.T) {
	manager, row, _ := queueGuardedRemote(t)
	require.False(t, manager.ReleaseRemoteRecoveryGuard("unknown-session"))
	require.True(t, manager.ReleaseRemoteRecoveryGuard(row.SessionID))
	require.NoError(t, manager.recoveryGuard.CheckLaunchAllowed(row.SessionID))
	require.False(t, manager.ReleaseRemoteRecoveryGuard(row.SessionID))
}

package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpawnSessionToolDescribesEffectiveAgentProfile(t *testing.T) {
	s := newTaskModeServer(t, &testBackend{}, "task-current")

	tool, ok := s.mcpServer.ListTools()["spawn_session_kandev"]
	require.True(t, ok, "spawn_session_kandev must be registered in task mode")

	description := tool.Tool.Description
	assert.Contains(t, description, "not a native subagent")
	assert.Contains(t, description, "only when the user explicitly requests another Kandev session")
	assert.Contains(t, description, "does not create a task")
	assert.NotContains(t, description, "pair of hands")
	assert.NotContains(t, description, "parallelizable piece")
	assert.Contains(t, description, "agent_profile_id")
	assert.Contains(t, description, "workflow step may override")
	assert.Contains(t, description, "Returns {task_id, session_id, state, agent_profile_id}")

	profileProperty, ok := toolInputProperties(t, s, "spawn_session_kandev")["agent_profile_id"].(map[string]interface{})
	require.True(t, ok, "agent_profile_id input must be described")
	assert.Contains(t, profileProperty["description"], "workflow launch profile may override it")
}

func TestSpawnSessionToolForwardsTheExactLaunchArguments(t *testing.T) {
	backend := &testBackend{response: map[string]interface{}{"session_id": "s-1"}}
	s := newTaskModeServer(t, backend, "task-current")

	callTool(t, s, "spawn_session_kandev", map[string]interface{}{
		"prompt": "go", "idempotency_key": "key-1", "make_primary": true, "expected_primary_session_id": "",
	})
	payload, ok := backend.lastPayload.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "key-1", payload["idempotency_key"])
	assert.Equal(t, true, payload["make_primary"])
	expected, present := payload["expected_primary_session_id"]
	assert.True(t, present, "an empty expected primary asserts there is none and must be forwarded")
	assert.Equal(t, "", expected)
	assert.Equal(t, "task-current", payload["sender_task_id"])
}

func TestSpawnSessionToolOmitsTheExactLaunchArgumentsWhenAbsent(t *testing.T) {
	backend := &testBackend{response: map[string]interface{}{"session_id": "s-1"}}
	s := newTaskModeServer(t, backend, "task-current")

	callTool(t, s, "spawn_session_kandev", map[string]interface{}{"prompt": "go", "name": "reviewer"})
	payload, ok := backend.lastPayload.(map[string]interface{})
	require.True(t, ok)
	for _, key := range []string{"idempotency_key", "make_primary", "expected_primary_session_id"} {
		assert.NotContains(t, payload, key)
	}
	assert.Equal(t, "reviewer", payload["name"])
}

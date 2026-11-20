package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const mib = int64(1024 * 1024)

// A Docker card gets 8 GiB unless its executor profile says otherwise, even
// when the agent definition still carries the old 4 GiB (here 256 MiB) limit.
func TestBuildContainerConfigDefaultsMemoryTo8GiB(t *testing.T) {
	cm := newCMTest(t)
	got, err := cm.buildContainerConfig(ContainerConfig{
		AgentConfig: newConfigStubAgent(), InstanceID: "0123456789abcdef", TaskID: "task-1",
	})
	require.NoError(t, err)
	require.Equal(t, 8192*mib, got.Memory)
	require.Equal(t, DefaultContainerMemoryMB*mib, got.Memory)
}

func TestBuildContainerConfigUsesProfileMemory(t *testing.T) {
	cm := newCMTest(t)
	got, err := cm.buildContainerConfig(ContainerConfig{
		AgentConfig: newConfigStubAgent(), InstanceID: "0123456789abcdef", TaskID: "task-1", MemoryMB: 12288,
	})
	require.NoError(t, err)
	require.Equal(t, 12288*mib, got.Memory)
}

// The profile value reaches the container config on both Docker runtimes.
func TestBuildDockerContainerConfigCarriesProfileMemory(t *testing.T) {
	for _, executorType := range []string{"local_docker", "remote_docker"} {
		t.Run(executorType, func(t *testing.T) {
			req := &ExecutorCreateRequest{
				InstanceID: "instance-1",
				TaskID:     "task-1",
				Metadata:   map[string]interface{}{MetadataKeyContainerMemoryMB: " 16384 "},
			}
			cfg, err := buildDockerContainerConfig(req, executorType)
			require.NoError(t, err)
			require.Equal(t, int64(16384), cfg.MemoryMB)

			req.Metadata[MetadataKeyContainerMemoryMB] = ""
			cfg, err = buildDockerContainerConfig(req, executorType)
			require.NoError(t, err)
			require.Zero(t, cfg.MemoryMB, "unset profile value must fall back to the default")
		})
	}
}

// A malformed or out-of-range value fails the launch instead of starving the
// card or leaving it effectively unlimited.
func TestBuildDockerContainerConfigRejectsBadProfileMemory(t *testing.T) {
	for _, raw := range []string{"8G", "8.5", "-1", "0", "256", "8589934592"} {
		t.Run(raw, func(t *testing.T) {
			req := &ExecutorCreateRequest{
				InstanceID: "instance-1",
				TaskID:     "task-1",
				Metadata:   map[string]interface{}{MetadataKeyContainerMemoryMB: raw},
			}
			_, err := buildDockerContainerConfig(req, "remote_docker")
			require.ErrorContains(t, err, MetadataKeyContainerMemoryMB)
		})
	}
}

func TestParseContainerMemoryMB(t *testing.T) {
	cases := map[string]int64{"": 0, "  ": 0, "512": 512, "8192": 8192, "1048576": 1048576}
	for raw, want := range cases {
		got, err := parseContainerMemoryMB(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
	_, err := parseContainerMemoryMB("1048577")
	require.Error(t, err)
}

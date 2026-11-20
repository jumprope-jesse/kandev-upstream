package executor

import (
	"testing"

	"github.com/kandev/kandev/internal/agent/runtime/lifecycle"
	"github.com/kandev/kandev/internal/task/models"
)

// The card memory limit comes only from the executor profile: a task cannot
// raise its own limit, and a profile with no value clears any task-supplied
// one so the runtime default applies.
func TestProfileConfigContainerMemoryIsAuthoritative(t *testing.T) {
	cases := []struct {
		name          string
		profileConfig map[string]string
		taskValue     interface{}
		want          string
	}{
		{name: "profile sets it", profileConfig: map[string]string{"container_memory_mb": "12288"}, want: "12288"},
		{name: "profile overrides task", profileConfig: map[string]string{"container_memory_mb": "8192"}, taskValue: "65536", want: "8192"},
		{name: "unset profile clears task value", profileConfig: map[string]string{}, taskValue: "65536", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metadata := map[string]interface{}{}
			if tc.taskValue != nil {
				metadata[lifecycle.MetadataKeyContainerMemoryMB] = tc.taskValue
			}
			applyProfileConfigToMetadata(string(models.ExecutorTypeRemoteDocker), tc.profileConfig, metadata)
			if got, _ := metadata[lifecycle.MetadataKeyContainerMemoryMB].(string); got != tc.want {
				t.Fatalf("metadata[%s] = %q, want %q", lifecycle.MetadataKeyContainerMemoryMB, got, tc.want)
			}
		})
	}
	for _, key := range profileConfigPassthroughKeys {
		if key == lifecycle.MetadataKeyContainerMemoryMB {
			t.Fatalf("%s must not be a passthrough key", key)
		}
	}
}

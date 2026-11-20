package docker

import (
	"testing"

	"github.com/moby/moby/api/types/mount"
)

func TestNewContainerHasRotatingLogsAndDiskBackedTemporaryScratch(t *testing.T) {
	cfg := buildHostConfig(ContainerConfig{}, nil, nil)
	if cfg.LogConfig.Type != "json-file" || cfg.LogConfig.Config["max-size"] != "100m" || cfg.LogConfig.Config["max-file"] != "3" {
		t.Fatalf("new container logs have no expected rotation budget: %#v", cfg.LogConfig)
	}
	if len(cfg.Tmpfs) != 0 {
		t.Fatalf("/tmp must not be a tmpfs (it counts against the container's memory limit): %#v", cfg.Tmpfs)
	}
	if !hasScratchVolume(cfg.Mounts) {
		t.Fatalf("new container has no per-container /tmp volume: %#v", cfg.Mounts)
	}
}

// hasScratchVolume reports an anonymous volume at /tmp: no source, so Docker
// creates one per container and removes it with the container.
func hasScratchVolume(mounts []mount.Mount) bool {
	for _, m := range mounts {
		if m.Type == mount.TypeVolume && m.Source == "" && m.Target == "/tmp" && !m.ReadOnly {
			return true
		}
	}
	return false
}

func TestExplicitTemporaryMountIsNotShadowed(t *testing.T) {
	cases := []struct {
		target      string
		wantScratch bool
	}{
		{target: "/tmp"},
		{target: "/tmp/"},
		{target: "//tmp"},
		{target: "/var/../tmp"},
		{target: "/var/tmp", wantScratch: true},
		{target: "/tmp/cache", wantScratch: true},
		{target: "/tmp-other", wantScratch: true},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			original := mount.Mount{Type: mount.TypeBind, Source: "/host/task-tmp", Target: tc.target, ReadOnly: true}
			cfg := buildHostConfig(ContainerConfig{}, []mount.Mount{original}, nil)
			if got := hasScratchVolume(cfg.Mounts); got != tc.wantScratch {
				t.Fatalf("default /tmp volume present = %v, want %v", got, tc.wantScratch)
			}
			if len(cfg.Tmpfs) != 0 {
				t.Fatalf("unexpected tmpfs: %#v", cfg.Tmpfs)
			}
			wantMounts := 1
			if tc.wantScratch {
				wantMounts = 2
			}
			if len(cfg.Mounts) != wantMounts || cfg.Mounts[0].Type != original.Type ||
				cfg.Mounts[0].Source != original.Source || cfg.Mounts[0].Target != original.Target ||
				cfg.Mounts[0].ReadOnly != original.ReadOnly {
				t.Fatalf("explicit mount changed: %#v", cfg.Mounts)
			}
		})
	}
}

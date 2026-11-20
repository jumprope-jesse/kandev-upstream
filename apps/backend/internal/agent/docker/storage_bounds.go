package docker

import (
	"path"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

// containerScratchTarget is the in-container scratch directory.
const containerScratchTarget = "/tmp"

// applyContainerStorageBounds limits disposable writes at container creation.
// Explicit task mounts retain their lifecycle and retention policy.
//
// /tmp is an anonymous Docker volume, so it lives on the daemon host's disk
// and goes away with the container (RemoveContainer removes volumes). It used
// to be a 2 GiB tmpfs, but tmpfs pages are charged to the container's memory
// cgroup: a card that filled /tmp with worktrees and build output had up to
// 2 GiB less memory for its jobs and was OOM-killed sooner. A new volume is
// seeded from the image's /tmp, so it keeps the image's 1777 mode.
func applyContainerStorageBounds(hc *container.HostConfig, mounts []mount.Mount) {
	hc.LogConfig = container.LogConfig{
		Type:   "json-file",
		Config: map[string]string{"max-size": "100m", "max-file": "3"},
	}
	for _, m := range mounts {
		if path.Clean(m.Target) == containerScratchTarget {
			return
		}
	}
	hc.Mounts = append(hc.Mounts, mount.Mount{Type: mount.TypeVolume, Target: containerScratchTarget})
}

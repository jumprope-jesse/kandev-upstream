package lifecycle

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// DefaultContainerMemoryMB is the Docker card memory limit when the
	// executor profile sets none. 4 GiB (the agents' old default) is not
	// enough for a full lint or test run of a large repository: the kernel
	// kills the job, and the agent can be killed with it.
	DefaultContainerMemoryMB int64 = 8192
	// minContainerMemoryMB and maxContainerMemoryMB bound a profile value so
	// a typo (MB entered as GB, or as bytes) fails the launch instead of
	// starving or effectively uncapping the container.
	minContainerMemoryMB int64 = 512
	maxContainerMemoryMB int64 = 1024 * 1024
)

// parseContainerMemoryMB reads the executor profile's container_memory_mb
// value. Empty means "not set" and returns 0.
func parseContainerMemoryMB(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	mb, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s = %q: want a whole number of MiB", MetadataKeyContainerMemoryMB, raw)
	}
	if mb < minContainerMemoryMB || mb > maxContainerMemoryMB {
		return 0, fmt.Errorf("%s = %d: want between %d and %d MiB",
			MetadataKeyContainerMemoryMB, mb, minContainerMemoryMB, maxContainerMemoryMB)
	}
	return mb, nil
}

// effectiveContainerMemoryMB applies the default to an unset profile value.
func effectiveContainerMemoryMB(profileMB int64) int64 {
	if profileMB > 0 {
		return profileMB
	}
	return DefaultContainerMemoryMB
}

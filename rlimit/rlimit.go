// Package rlimit allows raising RLIMIT_MEMLOCK if necessary for the use of BPF.
package rlimit

import (
	"errors"
	"fmt"
	"sync"

	"github.com/cilium/ebpf/internal"
	"github.com/cilium/ebpf/internal/sys"
	"github.com/cilium/ebpf/internal/unix"
)

var (
	unsupportedMemcgAccounting = &internal.UnsupportedFeatureError{
		MinimumVersion: internal.Version{5, 11, 0},
		Name:           "memcg-based accounting for BPF memory",
	}
	haveMemcgAccounting     error
	haveMemcgAccountingInit sync.Once

	rlimitMu sync.Mutex
)

func detectMemcgAccounting() error {
	// Retrieve the original limit to prevent lowering Max, since
	// doing so is a permanent operation when running unprivileged.
	var oldLimit unix.Rlimit
	if err := unix.Prlimit(0, unix.RLIMIT_MEMLOCK, nil, &oldLimit); err != nil {
		return fmt.Errorf("getting original memlock rlimit: %s", err)
	}

	// Drop the current limit to zero, maintaining the old Max value.
	// This is always permitted by the kernel for unprivileged users.
	// Retrieve a new copy of the old limit tuple to minimize the chances
	// of failing the restore operation below.
	zeroLimit := unix.Rlimit{Cur: 0, Max: oldLimit.Max}
	if err := unix.Prlimit(0, unix.RLIMIT_MEMLOCK, &zeroLimit, &oldLimit); err != nil {
		return fmt.Errorf("lowering memlock rlimit: %s", err)
	}

	attr := sys.MapCreateAttr{
		MapType:    2, /* Array */
		KeySize:    4,
		ValueSize:  4,
		MaxEntries: 1,
	}

	// Creating a map allocates shared (and locked) memory that counts against
	// the rlimit on pre-5.11 kernels, but against the memory cgroup budget on
	// kernels 5.11 and over. If this call succeeds with the process' memlock
	// rlimit set to 0, we can reasonably assume memcg accounting is supported.
	fd, mapErr := sys.MapCreate(&attr)

	// Restore old limits regardless of what happened.
	if err := unix.Prlimit(0, unix.RLIMIT_MEMLOCK, &oldLimit, nil); err != nil {
		return fmt.Errorf("restoring old memlock rlimit: %s", err)
	}

	// Map creation successful, memcg accounting supported.
	if mapErr == nil {
		fd.Close()
		return nil
	}

	// EPERM shows up when map creation would exceed the memory budget.
	if errors.Is(mapErr, unix.EPERM) {
		return unsupportedMemcgAccounting
	}

	// This shouldn't happen really.
	return fmt.Errorf("unexpected error detecting memory cgroup accounting: %s", mapErr)
}

// RemoveMemlock removes the limit on the amount of memory the current
// process can lock into RAM, if necessary.
//
// This is not required to load eBPF resources on kernel versions 5.11+
// due to the introduction of cgroup-based memory accounting. On such kernels
// the function is a no-op.
//
// Since the function may change global per-process limits it should be invoked
// at program start up, in main() or init().
//
// This function exists as a convenience and should only be used when
// permanently raising RLIMIT_MEMLOCK to infinite is appropriate. Consider
// invoking prlimit(2) directly with a more reasonable limit if desired.
//
// Requires CAP_SYS_RESOURCE on kernels < 5.11.
func RemoveMemlock() error {
	haveMemcgAccountingInit.Do(func() {
		haveMemcgAccounting = detectMemcgAccounting()
	})

	if haveMemcgAccounting == nil {
		return nil
	}

	if !errors.Is(haveMemcgAccounting, unsupportedMemcgAccounting) {
		return haveMemcgAccounting
	}

	rlimitMu.Lock()
	defer rlimitMu.Unlock()

	// pid 0 affects the current process. Requires CAP_SYS_RESOURCE.
	newLimit := unix.Rlimit{Cur: unix.RLIM_INFINITY, Max: unix.RLIM_INFINITY}
	if err := unix.Prlimit(0, unix.RLIMIT_MEMLOCK, &newLimit, nil); err != nil {
		return fmt.Errorf("failed to set memlock rlimit: %w", err)
	}

	return nil
}

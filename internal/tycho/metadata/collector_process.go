package metadata

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/casparwackerle/tycho-energy/pkg/cgroup"
	cfg "github.com/casparwackerle/tycho-energy/pkg/config"
	"github.com/casparwackerle/tycho-energy/pkg/utils"
)

// processCollector is responsible for discovering and updating process-level
// metadata (PID, container mapping, etc.).
//
// It does not interact with power/utilization collectors; it only maintains facts.
type processCollector struct {
	cfg   Config
	store *Store
}

// newProcessCollector constructs a processCollector bound to the shared Store.
func newProcessCollector(cfg Config, store *Store) *processCollector {
	return &processCollector{
		cfg:   cfg,
		store: store,
	}
}

// Collect refreshes process metadata at the given time.
//
// Parameters:
//   - ctx:  request context, for cancellation.
//   - ts:   aligned wall-clock time from the engine.
//   - mono: monotonic index derived from ts (0 if MonoSource not configured).
//
// Behavior:
//   - Enumerate /proc for numeric PIDs.
//   - For each PID:
//   - best-effort read of /proc/<pid>/comm
//   - resolve ContainerID via cgroup.GetContainerIDFromPID
//   - construct ProcMeta and upsert into the store.
//
// Notes:
//   - We deliberately do not attempt to track every short-lived process; very
//     short processes may never be sampled, which is acceptable for this
//     prototype.
//   - Kernel vs. system vs. user processes are currently differentiated via
//     ContainerID == utils.SystemProcessName (system/kernel-ish) vs. a real
//     container ID. More nuanced classification can be added later.
func (pc *processCollector) Collect(ctx context.Context, ts time.Time, mono uint64) {
	procDir := cfg.ProcDir()
	entries, err := os.ReadDir(procDir)
	if err != nil {
		klog.Errorf("[metadata/process] failed to read proc dir %q: %v", procDir, err)
		return
	}

	for _, ent := range entries {
		select {
		case <-ctx.Done():
			klog.V(3).Infof("[metadata/process] context cancelled, stopping scan early")
			return
		default:
		}

		if !ent.IsDir() {
			continue
		}

		pid, err := strconv.ParseUint(ent.Name(), 10, 64)
		if err != nil {
			continue // skip non-PID entries like "net", "self"
		}

		command := readProcComm(procDir, ent.Name())

		// NEW: read start_time (in kernel ticks) from /proc/<pid>/stat
		startTimeTicks := readProcStartTimeTicks(procDir, ent.Name())

		containerID, err := cgroup.GetContainerIDFromPID(pid)
		if err != nil {
			klog.V(6).Infof("[metadata/process] GetContainerIDFromPID(%d) error: %v", pid, err)
		}
		if containerID == "" {
			containerID = utils.SystemProcessName
		}

		meta := &ProcMeta{
			PID:          pid,
			StartJiffies: startTimeTicks, // actually kernel ticks; see comment in ProcMeta
			CgroupID:     0,              //correlated directly with eBPF PID to cgroupID matching
			ContainerID:  containerID,
			Command:      command,
			LastSeenMono: mono,
			LastSeenWall: ts,
		}

		pc.store.UpsertProc(meta)
	}
}

// readProcComm reads /proc/<pid>/comm and returns a trimmed command name.
// Returns an empty string on error.
func readProcComm(procDir, pidStr string) string {
	path := filepath.Join(procDir, pidStr, "comm")
	data, err := os.ReadFile(path)
	if err != nil {
		// Common for very short-lived processes or permission issues.
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readProcStartTimeTicks reads the start_time field from /proc/<pid>/stat
// and returns it as a uint64. The value is in kernel clock ticks (jiffies).
//
// We only use this as a stable per-boot identifier to distinguish different
// incarnations of the same PID, so the exact time unit does not matter.
func readProcStartTimeTicks(procDir, pidStr string) uint64 {
	path := filepath.Join(procDir, pidStr, "stat")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	text := string(data)

	// /proc/<pid>/stat format:
	// pid (comm with spaces) S ... 22nd field = start_time
	//
	// The comm field may contain spaces, so we find the last ')' and
	// then field-split the substring after it.
	rparen := strings.LastIndex(text, ")")
	if rparen < 0 || rparen+2 >= len(text) {
		return 0
	}

	// Skip ") " and split the remainder into fields.
	fields := strings.Fields(text[rparen+2:])
	// After skipping "pid (comm) ", the first field in 'fields' is stat field 3.
	// start_time is field 22, so index = 22 - 3 = 19.
	const startFieldIndex = 19
	if len(fields) <= startFieldIndex {
		return 0
	}

	startStr := fields[startFieldIndex]
	startTicks, err := strconv.ParseUint(startStr, 10, 64)
	if err != nil {
		return 0
	}

	return startTicks
}

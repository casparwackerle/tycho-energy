package attribution

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/metadata"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/utils"
)

// BuildCgroupIndexFromBpfWindow refreshes the store's cgroupID->containerID index
// using the current BPF window plus existing ProcMeta(pid->containerID) facts.
//
// Join logic (hardened against PID reuse):
//   - For each (pid,cgroupID) observed in the BPF window,
//   - Look up ProcMeta(pid). If it has a non-system ContainerID,
//   - Validate ProcMeta.StartJiffies matches the current /proc/<pid>/stat starttime,
//   - Upsert mapping (cgroupID -> containerID).
//
// This is intentionally called once per analysis window (not per eBPF tick).
func BuildCgroupIndexFromBpfWindow(
	store *metadata.Store,
	nowWall time.Time,
	nowMono uint64,
	ticks []ring.BpfTick,
) {
	if store == nil || len(ticks) == 0 {
		return
	}

	// De-dup by PID for the window. Last cgroupID wins (usually stable anyway).
	pidToCgid := make(map[uint64]uint64, 1024)
	for i := range ticks {
		t := &ticks[i]
		for j := range t.Procs {
			p := &t.Procs[j]
			if p.PID == 0 || !IsAttributableCgroupID(p.CgroupID) {
				continue
			}
			pidToCgid[p.PID] = p.CgroupID
		}
	}

	// Join: PID -> ContainerID (from proc scan) and store cgroup mapping.
	for pid, cgid := range pidToCgid {
		if !IsAttributableCgroupID(cgid) {
			continue
		}

		pm, ok := store.LookupProc(pid)
		if !ok || pm == nil {
			continue
		}
		if pm.ContainerID == "" || pm.ContainerID == utils.SystemProcessName {
			continue
		}

		// PID reuse hardening:
		// Only accept PID->container facts if the process instance matches.
		//
		// If StartJiffies is missing/zero, be conservative and DO NOT upsert.
		// This avoids poisoning the cgroup index with stale ProcMeta entries.
		if pm.StartJiffies == 0 {
			continue
		}
		curStart, err := readStartJiffiesFromProc(uint64(pid))
		if err != nil || curStart == 0 {
			continue
		}
		if curStart != pm.StartJiffies {
			// Stale ProcMeta for a reused PID -> ignore.
			continue
		}

		// Store boundary will also reject non-attributable cgroup IDs, but keep guard here too.
		store.UpsertCgroupMapping(cgid, pm.ContainerID, nowMono, nowWall)
	}
}

// readStartJiffiesFromProc reads Linux /proc/<pid>/stat field 22 (starttime in jiffies).
func readStartJiffiesFromProc(pid uint64) (uint64, error) {
	f, err := os.Open("/proc/" + strconv.FormatUint(pid, 10) + "/stat")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	rd := bufio.NewReader(f)
	line, err := rd.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return 0, errors.New("empty /proc/<pid>/stat")
	}

	// /proc/<pid>/stat has comm in parentheses (may contain spaces).
	rp := strings.LastIndexByte(line, ')')
	if rp < 0 || rp+2 >= len(line) {
		return 0, errors.New("bad /proc/<pid>/stat format")
	}
	rest := line[rp+2:] // skip ") "
	fields := strings.Fields(rest)

	// starttime is field #22 overall => index 19 in "rest"
	const idx = 19
	if len(fields) <= idx {
		return 0, errors.New("stat too short")
	}

	v, err := strconv.ParseUint(fields[idx], 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

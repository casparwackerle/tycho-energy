// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
// Copyright 2021.

#pragma once

typedef unsigned char __u8;
typedef short int __s16;
typedef short unsigned int __u16;
typedef int __s32;
typedef unsigned int __u32;
typedef long long int __s64;
typedef long long unsigned int __u64;
typedef __u8 u8;
typedef __s16 s16;
typedef __u16 u16;
typedef __s32 s32;
typedef __u32 u32;
typedef __s64 s64;
typedef __u64 u64;
typedef __u16 __le16;
typedef __u16 __be16;
typedef __u32 __be32;
typedef __u64 __be64;
typedef __u32 __wsum;
typedef int pid_t;
typedef struct pid_time_t {
	__u32 pid;
} pid_time_t;

#ifndef NUM_CPUS
# define NUM_CPUS 128
#endif

#ifndef MAP_SIZE
# define MAP_SIZE 32768
#endif

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>

enum bpf_map_type {
	BPF_MAP_TYPE_UNSPEC = 0,
	BPF_MAP_TYPE_HASH = 1,
	BPF_MAP_TYPE_ARRAY = 2,
	BPF_MAP_TYPE_PROG_ARRAY = 3,
	BPF_MAP_TYPE_PERF_EVENT_ARRAY = 4,
	BPF_MAP_TYPE_PERCPU_HASH = 5,
	BPF_MAP_TYPE_PERCPU_ARRAY = 6,
	BPF_MAP_TYPE_STACK_TRACE = 7,
	BPF_MAP_TYPE_CGROUP_ARRAY = 8,
	BPF_MAP_TYPE_LRU_HASH = 9,
	BPF_MAP_TYPE_LRU_PERCPU_HASH = 10,
	BPF_MAP_TYPE_LPM_TRIE = 11,
	BPF_MAP_TYPE_ARRAY_OF_MAPS = 12,
	BPF_MAP_TYPE_HASH_OF_MAPS = 13,
	BPF_MAP_TYPE_DEVMAP = 14,
	BPF_MAP_TYPE_SOCKMAP = 15,
	BPF_MAP_TYPE_CPUMAP = 16,
	BPF_MAP_TYPE_XSKMAP = 17,
	BPF_MAP_TYPE_SOCKHASH = 18,
	BPF_MAP_TYPE_CGROUP_STORAGE = 19,
	BPF_MAP_TYPE_REUSEPORT_SOCKARRAY = 20,
	BPF_MAP_TYPE_PERCPU_CGROUP_STORAGE = 21,
	BPF_MAP_TYPE_QUEUE = 22,
	BPF_MAP_TYPE_STACK = 23,
	BPF_MAP_TYPE_SK_STORAGE = 24,
	BPF_MAP_TYPE_DEVMAP_HASH = 25,
	BPF_MAP_TYPE_STRUCT_OPS = 26,
	BPF_MAP_TYPE_RINGBUF = 27,
	BPF_MAP_TYPE_INODE_STORAGE = 28,
};

enum {
	BPF_ANY = 0,
	BPF_NOEXIST = 1,
	BPF_EXIST = 2,
	BPF_F_LOCK = 4,
};

enum {
	BPF_F_INDEX_MASK = 0xffffffffULL,
	BPF_F_CURRENT_CPU = BPF_F_INDEX_MASK,
	/* BPF_FUNC_perf_event_output for sk_buff input context. */
	BPF_F_CTXLEN_MASK = (0xfffffULL << 32),
};

struct bpf_perf_event_value {
	__u64 counter;
	__u64 enabled;
	__u64 running;
};

// struct {
//   __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
//   __type(key, u32);
//   __type(value, u64);
//   __uint(max_entries, 1);
// } perf_read_err_cycles SEC(".maps");

// struct {
//   __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
//   __type(key, u32);
//   __type(value, u64);
//   __uint(max_entries, 1);
// } perf_read_err_instr SEC(".maps");

// struct {
//   __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
//   __type(key, u32);
//   __type(value, u64);
//   __uint(max_entries, 1);
// } perf_read_err_miss SEC(".maps");



/* ---- Kernel/thread classification ---- */
/* PF_KTHREAD from linux/sched.h (bit 22). Using a literal keeps us BTF-agnostic. */
#ifndef PF_KTHREAD
#define PF_KTHREAD (0x00200000)
#endif

/* Optional special keys for system buckets if ever needed in a hash map */
enum system_bucket_key {
	SYS_IDLE    = 1,
	SYS_IRQ     = 2,
	SYS_SOFTIRQ = 3,
	SYS_KTHREAD = 4,
};

/* ---- Per-CPU scheduler/IRQ state (hot path) ---- */
struct cpu_state_t {
	/* Timestamp (ns) of last sched_switch accounting on this CPU */
	u64 last_ts;

	/* Currently running pid on this CPU (0 == idle task) */
	u32 current_pid;

	/* Per-bin accumulators (ns) on this CPU */
	u64 idle_ns;
	u64 irq_ns;
	u64 softirq_ns;

	/* In-flight timing for IRQ and SoftIRQ on this CPU (ns) */
	u64 irq_entry_ts;
	u64 softirq_entry_ts;
};

/* One entry per CPU, no contention between CPUs */
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__type(key, u32);
	__type(value, struct cpu_state_t);
	__uint(max_entries, NUM_CPUS);
} cpu_states SEC(".maps");

/* ---- Per-CPU bin counters (resettable by userspace) ----
 * One entry (key=0), but PERCPU, so each CPU has its own copy.
 * Userspace: read & zero every 50 ms (aligned to RAPL bin).
 */
struct cpu_bin_counters {
	u64 idle_ns;
	u64 irq_ns;
	u64 softirq_ns;
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__type(key, u32);
	__type(value, struct cpu_bin_counters);
	__uint(max_entries, 1);
} cpu_bins SEC(".maps");

/* Small helper to get this CPU's bin */
static __always_inline struct cpu_bin_counters *get_cpu_bin(void)
{
	u32 zero = 0;
	return bpf_map_lookup_elem(&cpu_bins, &zero);
}

typedef struct process_metrics_t {
	u64 cgroup_id;
	u64 pid; // pid is the kernel space view of the thread id
	__u8 is_kthread;   /* 1 if PF_KTHREAD observed for this TGID at least once */
	__u8 _pad_[7];     /* pad to 8-byte alignment for the next u64 */
	u64 process_run_time;
	u64 cpu_cycles;
	u64 cpu_instr;
	u64 cache_miss;
	u64 page_cache_hit;
	u16 vec_nr[10];
	char comm[16];
} process_metrics_t;


struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__type(key, u32);
	__type(value, process_metrics_t);
	__uint(max_entries, MAP_SIZE);
} processes SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__type(key, u32);
	__type(value, u64);
	__uint(max_entries, MAP_SIZE);
} pid_time_map SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
	__type(key, int);
	__type(value, u32);
	__uint(max_entries, NUM_CPUS);
} cpu_cycles_event_reader SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, u32);
	__type(value, u64);
	__uint(max_entries, NUM_CPUS);
} cpu_cycles SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
	__type(key, int);
	__type(value, u32);
	__uint(max_entries, NUM_CPUS);
} cpu_instructions_event_reader SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, u32);
	__type(value, u64);
	__uint(max_entries, NUM_CPUS);
} cpu_instructions SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERF_EVENT_ARRAY);
	__type(key, int);
	__type(value, u32);
	__uint(max_entries, NUM_CPUS);
} cache_miss_event_reader SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, u32);
	__type(value, u64);
	__uint(max_entries, NUM_CPUS);
} cache_miss SEC(".maps");

// Test mode skips unsupported helpers
SEC(".rodata.config")
__attribute__((btf_decl_tag("Test"))) static volatile const int TEST = 0;

// Test mode skips unsupported helpers
SEC(".rodata.config")
__attribute__((btf_decl_tag(
	"Hardware Events Enabled"))) static volatile const int HW = 1;

// The sampling rate should be disabled by default because its impact on the
// measurements is unknown.
SEC(".rodata.config")
__attribute__((
	btf_decl_tag("Sample Rate"))) static volatile const int SAMPLE_RATE = 0;

int counter_sched_switch = 0;

struct task_struct {
	int pid;
	unsigned int tgid;
	unsigned int flags;
} __attribute__((preserve_access_index));


static inline u64 calc_delta(u64 *prev_val, u64 val)
{
	u64 delta = 0;
	// Probably a clock issue where the recorded on-CPU event had a
	// timestamp later than the recorded off-CPU event, or vice versa.
	if (prev_val && val > *prev_val)
		delta = val - *prev_val;

	return delta;
}

static inline u64 get_on_cpu_elapsed_time_us(u32 prev_pid, u64 curr_ts)
{
	u64 cpu_time = 0;
	u64 *prev_ts;

	prev_ts = bpf_map_lookup_elem(&pid_time_map, &prev_pid);
	if (prev_ts) {
		cpu_time = calc_delta(prev_ts, curr_ts) / 1000;
		bpf_map_delete_elem(&pid_time_map, &prev_pid);
	}

	return cpu_time;
}

static inline u64 get_on_cpu_cycles(u32 *cpu_id)
{
	u64 delta, val, *prev_val;
	long error;
	struct bpf_perf_event_value c = {};

	error = bpf_perf_event_read_value(
		&cpu_cycles_event_reader, *cpu_id, &c, sizeof(c));
	if (error) {
		// u32 k = 0;
		// u64 *errcnt = bpf_map_lookup_elem(&perf_read_err_cycles, &k);
		// if (errcnt) {
		// 	__sync_fetch_and_add(errcnt, 1);
		// }
		return 0;
	}


	val = c.counter;
	prev_val = bpf_map_lookup_elem(&cpu_cycles, cpu_id);
	delta = calc_delta(prev_val, val);
	bpf_map_update_elem(&cpu_cycles, cpu_id, &val, BPF_ANY);

	return delta;
}


static inline u64 get_on_cpu_instr(u32 *cpu_id)
{
	u64 delta, val, *prev_val;
	long error;
	struct bpf_perf_event_value c = {};

	error = bpf_perf_event_read_value(
		&cpu_instructions_event_reader, *cpu_id, &c, sizeof(c));
	if (error) {
		// u32 k = 0;
		// u64 *errcnt = bpf_map_lookup_elem(&perf_read_err_instr, &k);
		// if (errcnt) {
		// 	__sync_fetch_and_add(errcnt, 1);
		// }
		return 0;
	}


	val = c.counter;
	prev_val = bpf_map_lookup_elem(&cpu_instructions, cpu_id);
	delta = calc_delta(prev_val, val);
	bpf_map_update_elem(&cpu_instructions, cpu_id, &val, BPF_ANY);

	return delta;
}


static inline u64 get_on_cpu_cache_miss(u32 *cpu_id)
{
	u64 delta, val, *prev_val;
	long error;
	struct bpf_perf_event_value c = {};

	error = bpf_perf_event_read_value(
		&cache_miss_event_reader, *cpu_id, &c, sizeof(c));
	if (error) {
		// u32 k = 0;
		// u64 *errcnt = bpf_map_lookup_elem(&perf_read_err_miss, &k);
		// if (errcnt) {
		// 	__sync_fetch_and_add(errcnt, 1);
		// }
		return 0;
	}


	val = c.counter;
	prev_val = bpf_map_lookup_elem(&cache_miss, cpu_id);
	delta = calc_delta(prev_val, val);
	bpf_map_update_elem(&cache_miss, cpu_id, &val, BPF_ANY);

	return delta;
}


static inline void register_new_process_if_not_exist(u32 tgid)
{
	u64 cgroup_id;
	struct process_metrics_t *curr_tgid_metrics;

	// create new process metrics
	curr_tgid_metrics = bpf_map_lookup_elem(&processes, &tgid);
	if (!curr_tgid_metrics) {
		cgroup_id = bpf_get_current_cgroup_id();
		// the Kernel tgid is the user-space PID, and the Kernel pid is the
		// user-space TID
		process_metrics_t new_process = {
			.pid = tgid,
			.cgroup_id = cgroup_id,
		};

		if (!TEST)
			bpf_get_current_comm(
				&new_process.comm, sizeof(new_process.comm));

		bpf_map_update_elem(&processes, &tgid, &new_process, BPF_NOEXIST);
	}
}

static inline void collect_metrics_and_reset_counters(
	struct process_metrics_t *buf, u32 prev_pid, u64 curr_ts, u32 cpu_id)
{
	if (HW) {
		buf->cpu_cycles = get_on_cpu_cycles(&cpu_id);
		buf->cpu_instr = get_on_cpu_instr(&cpu_id);
		buf->cache_miss = get_on_cpu_cache_miss(&cpu_id);
	}
	// Get current time to calculate the previous task on-CPU time
	buf->process_run_time = get_on_cpu_elapsed_time_us(prev_pid, curr_ts);
}

static inline void do_page_cache_hit_increment(u32 curr_pid)
{
	struct process_metrics_t *process_metrics;

	process_metrics = bpf_map_lookup_elem(&processes, &curr_pid);
	if (process_metrics)
		process_metrics->page_cache_hit++;
}

static inline int do_kepler_sched_switch_trace(
	u32 prev_pid, u32 next_pid, u32 prev_tgid, u32 next_tgid)
{
	u32 cpu_id;
	u64 curr_ts = bpf_ktime_get_ns();

	struct process_metrics_t *curr_tgid_metrics, *prev_tgid_metrics;
	struct process_metrics_t buf = {};

	cpu_id = bpf_get_smp_processor_id();

	// Skip some samples to minimize overhead (unchanged)
	if (SAMPLE_RATE > 0) {
		if (counter_sched_switch > 0) {
			// update hardware counters to be used when sample is taken
			if (counter_sched_switch == 1) {
				// collect deltas for the task that just ran (prev)
				collect_metrics_and_reset_counters(&buf, prev_pid, curr_ts, cpu_id);

				// Start timing for the task that is about to run (next)
				bpf_map_update_elem(&pid_time_map, &next_pid, &curr_ts, BPF_ANY);

				// IMPORTANT: Register the NEXT task with cgroup_id (ONLY next)
				if (next_tgid != 0) {
					register_new_process_if_not_exist(next_tgid);
				}
			}
			counter_sched_switch--;
			return 0;
		}
		counter_sched_switch = SAMPLE_RATE;
	}

	// Collect PMU deltas + on-CPU time for the task that just ran (prev)
	collect_metrics_and_reset_counters(&buf, prev_pid, curr_ts, cpu_id);

	// If we have valid on-CPU time, add it (and PMU deltas) to prev_tgid
	if (buf.process_run_time > 0) {
		prev_tgid_metrics = bpf_map_lookup_elem(&processes, &prev_tgid);
		if (prev_tgid_metrics) {
			prev_tgid_metrics->process_run_time += buf.process_run_time;
			prev_tgid_metrics->cpu_cycles       += buf.cpu_cycles;
			prev_tgid_metrics->cpu_instr        += buf.cpu_instr;
			prev_tgid_metrics->cache_miss       += buf.cache_miss;
		} else {
			/*
			 * IMPORTANT: Do NOT call register_new_process_if_not_exist(prev_tgid)
			 * here, because that helper uses bpf_get_current_cgroup_id(), which
			 * at sched_switch refers to the *next* task’s cgroup. That would
			 * mislabel the prev task.
			 *
			 * Instead, create a minimal slot for prev_tgid WITHOUT cgroup_id.
			 * This allows us to accumulate on the next switch-out safely.
			 */
// struct process_metrics_t init = {};
// init.pid = prev_tgid;   // leave cgroup_id as 0 (unknown)
// // (We also skip comm here to avoid capturing the next task’s name.)
// bpf_map_update_elem(&processes, &prev_tgid, &init, BPF_NOEXIST);
			/* Entry missing (often due to userspace read/delete). Do NOT drop deltas. */
			struct process_metrics_t init = {};
			init.pid = prev_tgid;          /* cgroup_id unknown here, keep 0 */
			init.process_run_time = buf.process_run_time;
			init.cpu_cycles       = buf.cpu_cycles;
			init.cpu_instr        = buf.cpu_instr;
			init.cache_miss       = buf.cache_miss;
			bpf_map_update_elem(&processes, &prev_tgid, &init, BPF_NOEXIST);
		}
	}

	// IMPORTANT: Register ONLY the NEXT task with cgroup_id (correct timing)
	if (next_tgid != 0) {
		register_new_process_if_not_exist(next_tgid);
	}

	// Start timing for the task that is about to run (next)
	curr_ts = bpf_ktime_get_ns();
	bpf_map_update_elem(&pid_time_map, &next_pid, &curr_ts, BPF_ANY);

	return 0;
}


static __always_inline void *
bpf_map_lookup_or_try_init(void *map, const void *key, const void *init)
{
	void *val;
	int err;

	val = bpf_map_lookup_elem(map, key);
	if (val)
		return val;

	err = bpf_map_update_elem(map, key, init, BPF_NOEXIST);
	if (err && err != -17)
		return 0;

	return bpf_map_lookup_elem(map, key);
}

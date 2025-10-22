// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
// Copyright 2021.

#include "kepler.bpf.h"

static __always_inline struct cpu_state_t *get_cpu_state(void)
{
	u32 cpu = bpf_get_smp_processor_id();
	return bpf_map_lookup_elem(&cpu_states, &cpu);
}

SEC("tp_btf/sched_switch")
int kepler_sched_switch_trace(u64 *ctx)
{
	/* BTF args: (prev, next) as task_struct* at ctx[1], ctx[2] */
	struct task_struct *prev_task = (struct task_struct *)ctx[1];
	struct task_struct *next_task = (struct task_struct *)ctx[2];

	u32 prev_pid = (u32)prev_task->pid;
	u32 next_pid = (u32)next_task->pid;
	u32 prev_tgid = (u32)prev_task->tgid;
	u32 next_tgid = (u32)next_task->tgid;

	u64 now = bpf_ktime_get_ns();

	/* --- Per-CPU state for idle/irq/softirq time accounting --- */
	struct cpu_state_t *st = get_cpu_state();
	if (st) {
		/* Attribute time since last switch to whoever was running */
		if (st->last_ts > 0) {
			u64 dt = now - st->last_ts;
			/* If the CPU was previously running idle task */
			if (st->current_pid == 0) {
				st->idle_ns += dt;
			}
			/* else: user/kernel on-CPU time is handled by existing logic below */
		}
		/* Update current running entity and timestamp */
		st->current_pid = next_pid;   /* 0 means entering idle */
		st->last_ts = now;
	}

	/* --- Existing KEPLER logic (preserved) --- */
	return do_kepler_sched_switch_trace(prev_pid, next_pid, prev_tgid, next_tgid);
}

/* SoftIRQ timing (entry/exit) + keep your existing vec count */
SEC("tp_btf/softirq_entry")
int kepler_softirq_entry(u64 *ctx)
{
	/* Existing vector count for enrichment */
	u32 curr_tgid = bpf_get_current_pid_tgid() >> 32;
	struct process_metrics_t *pm = bpf_map_lookup_elem(&processes, &curr_tgid);
	unsigned int vec = (unsigned int)ctx[0];
	if (pm && vec < 10) {
		pm->vec_nr[vec] += 1;
	}

	/* Time accounting: start softirq interval for this CPU */
	struct cpu_state_t *st = get_cpu_state();
	if (st) {
		/* Only set if not already in a softirq (nested softirqs are rare; we measure outermost) */
		if (st->softirq_entry_ts == 0)
			st->softirq_entry_ts = bpf_ktime_get_ns();
	}
	return 0;
}

SEC("tp_btf/softirq_exit")
int kepler_softirq_exit(u64 *ctx)
{
	struct cpu_state_t *st = get_cpu_state();
	if (st && st->softirq_entry_ts) {
		u64 now = bpf_ktime_get_ns();
		u64 dt = now - st->softirq_entry_ts;
		st->softirq_ns += dt;
		st->softirq_entry_ts = 0;
	}
	return 0;
}

/* Hard IRQ timing (entry/exit) */
SEC("tp_btf/irq_handler_entry")
int kepler_irq_entry(u64 *ctx)
{
	struct cpu_state_t *st = get_cpu_state();
	if (st) {
		if (st->irq_entry_ts == 0)
			st->irq_entry_ts = bpf_ktime_get_ns();
	}
	return 0;
}

SEC("tp_btf/irq_handler_exit")
int kepler_irq_exit(u64 *ctx)
{
	struct cpu_state_t *st = get_cpu_state();
	if (st && st->irq_entry_ts) {
		u64 now = bpf_ktime_get_ns();
		u64 dt = now - st->irq_entry_ts;
		st->irq_ns += dt;
		st->irq_entry_ts = 0;
	}
	return 0;
}

/* count read page cache */
SEC("fexit/mark_page_accessed")
int kepler_read_page_trace(void *ctx)
{
	u32 curr_tgid = bpf_get_current_pid_tgid() >> 32;
	do_page_cache_hit_increment(curr_tgid);
	return 0;
}

/* count write page cache */
SEC("tp/writeback_dirty_folio")
int kepler_write_page_trace(void *ctx)
{
	u32 curr_tgid = bpf_get_current_pid_tgid() >> 32;
	do_page_cache_hit_increment(curr_tgid);
	return 0;
}

char __license[] SEC("license") = "Dual BSD/GPL";

# Guardrails: keep machines alive without hoarding

Agent work must never lock up the machine it runs on, above all a bare-metal box with no
console to recover from. It must also be free to use nearly all of the hardware the rest
of the time. Syre does both with **survival-reserve, pressure-driven guardrails**: soft
kernel signals plus a daemon watchdog that sheds the newest session when the machine runs
short. There are no hard per-session caps.

Code: `internal/worker/guard` (syred), `internal/controller/guardrails.go` (hub).

## What Syre never does

| Never | Why |
|---|---|
| A hard per-session memory cap (`MemoryMax` per scope, `--memory` per container) | A 200 MiB job holds a slice it doesn't use, and a bursty linker is SIGKILLed on a brief spike. |
| `MemoryMin` / `MemoryLow` on worker cgroups | Guarantees reserve RAM for workers that may be idle. |
| `JOB_OBJECT_LIMIT_JOB_MEMORY` / `_PROCESS_MEMORY` on Windows | They cap *committed* memory: JVM, .NET, V8/Node and Playwright crash while physical RAM is free. |
| `RLIMIT_AS` | Same commit-vs-physical problem. |
| Per-session reservations in the capacity model | Estimates are fine for placement; reservations hoard. |
| `CPUQuota` / a hard CPU rate unless a throttle asks for one | A quota caps CPU even when the machine is idle. CPU *weight* only matters under contention. |

Fork-bomb guards that hoard nothing (`TasksMax=16384` on the shared slice) are fine.

## Survival reserve

```
SurvivalReserve = clamp(5% of RAM, 96 MiB, 1.5 GiB)   (cap 768 MiB on a headless host)
                  and never more than half of RAM
```

| Host | Reserve | Left for work |
|---|---|---|
| 1 GiB VM | 96 MiB | ~928 MiB |
| 8 GiB laptop | ~410 MiB | ~7.6 GiB |
| 16 GiB Mac | ~819 MiB | ~15.2 GiB |
| 32 GiB workstation | 1.5 GiB | 30.5 GiB |
| 32 GiB headless server | 768 MiB | 31.25 GiB |

The reserve is withheld from Syre's workers only. The OS, sshd, the desktop and page cache
can still use it. All managed sessions share one pool, `TotalRAM − SurvivalReserve`:
nothing is sliced per session.

"Desktop" means macOS, Windows client editions, and Linux with an X11 or Wayland session.

## Modes

| Mode | What it does | Default for |
|---|---|---|
| `passthrough` | Track sessions (process group / Job Object) and clean up orphans. No watchdog, no nested limits. | nothing (opt-in) |
| `adaptive` | Plus the pressure watchdog and worker deprioritisation. No kernel watermarks. | VMs, WSL2 (they have an out-of-band reboot) and containers |
| `adaptive_strict` | Plus soft kernel watermarks (the Linux slice's `MemoryHigh` and swap cap). | bare metal (a lockup needs a power cycle) |

Precedence: the newest active `machine.guardrails` directive (hub) > syred config
`guardrails:` / `--guardrails` > auto (from `MachineFacts.Virtualization`). Machine facts
report the effective mode, its source, the base mode (without directives), the mechanism
and the reserve. The machine view and `syred doctor` show them.

`hermetic_container` (opt-in per-project container execution) is planned separately
(SYR-4HJ5Q) and is not a mode yet.

## Per OS

### Linux

Every managed session (agent task PTY, chat engine, verify command) runs in its own
transient user scope, `syre-<kind>-<syred pid>-<n>.scope`. All the scopes sit inside one
shared `syre-workers.slice`. Syred launches through
`systemd-run --user --scope --slice=syre-workers.slice`, which registers the scope and
execs the command, so the PID and process group are unchanged.

Limits live on the **slice**, never on a scope (`systemctl --user set-property --runtime`):

| Property | adaptive_strict | adaptive |
|---|---|---|
| `MemoryHigh` | `Total − Reserve` (soft: reclaim and throttle, never kill) | `infinity` |
| `MemorySwapMax` | `min(25% RAM, 2 GiB)` | `infinity` |
| `MemoryMax` | `infinity` | `infinity` |
| `CPUWeight` | 100, or 20 while `machine.throttle` is active | same |
| `CPUQuota` | only for `cpu_ceiling_pct` (× cores) | same |
| `TasksMax` | 16384 | same |

`MemoryHigh` is a ceiling on all workers together, not a reservation. Idle workers hold
nothing. One job can use the whole pool when it is alone.

A **pool stall** watch covers the case the ceiling creates. (Live on bocaminipc-wsl,
forced strict, the below-reserve trip fired first: `MemoryHigh` is soft and the host's
own ~1 GiB of non-worker memory puts workers at the ceiling below the reserve already.
The stall trip only leads when almost nothing else uses memory.) Workers can't push host
`MemAvailable` below the reserve, so a job that genuinely needs more than the ceiling
would crawl in reclaim forever. The watchdog reads the slice's `memory.pressure`. When
"full" avg10 stays ≥ 20% for 5 s, it sheds the newest session.

Freeze and kill use `cgroup.freeze` and `cgroup.kill` on the scope (the whole tree, even
processes that left the group), with the process group as a fallback. The peak comes from
the scope's `memory.peak`, sampled while it runs, because a collected scope loses it at exit.

At startup syred stops `syre-*.scope` units whose syred is gone. Scopes live outside
syred's service cgroup, so they would otherwise outlive a crash.

**Containers.** The runtime's limit is a cgroup `memory.max`, and its OOM killer fires
there, whatever the host has free. It picks the biggest process, which may be the agent,
and the run then dies as an ordinary failure instead of a requeued shed. So the guard
runs `adaptive` in containers and judges against the tightest numeric `memory.max` on the
path from the workers' cgroup (the worker slice, else syred's own) up to the root: the
total is that limit (machine facts report it as the RAM total), and available memory is
the limit minus the cgroup's working set (`memory.current` less `inactive_file`, which the
kernel reclaims before it OOM-kills), or the host's `MemAvailable` if that is lower.
A container limit is a hard wall with no reclaim or swap to slow an allocator, so the
500 ms debounce does not apply under it: available memory below the reserve sheds at
once, and so does a decline that would cross the reserve before the next poll. Live, in a
1 GiB podman container, a hog growing 1.3 GB/s was shed at 827 MiB; without the trend it
reached the limit inside the debounce and the container's OOM killer took it.
The same walk applies a `MemoryMax` set on syred's own service. Workers get
`oom_score_adj +300` as anywhere else, so when the OOM killer does fire it prefers them.

**Fallback** when there is no usable user manager (minimal WSL2, no session bus, cgroup v1):
process trees as on macOS (below), plus the watchdog on `/proc/meminfo`. Throttle renices
the group.

### Windows

One **Job Object per managed session**, in every mode, including chat engines and ticket runs:

- `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`: no orphans. Releasing the session closes the job,
  which ends anything the root left behind.
- CPU rate control, weight-based: 5 normally, 1 throttled. A hard rate only for
  `cpu_ceiling_pct`.
- Priority class below normal while throttled.
- Basic UI restrictions for headless ticket runs only (no logoff, display or
  system-parameter changes, no desktop switching). Clipboard and handles stay open so
  agents that drive a browser keep working.
- **Never** a job or process memory limit.

The watchdog reads `GlobalMemoryStatusEx`: available physical memory, and the commit
headroom (`ullAvailPageFile`, the commit limit minus the commit charge). Physical memory
is the right measure for JVM and Node reservations, but Windows also has a hard commit
limit (RAM plus page file): when it runs out, every allocation on the machine fails,
syred's and Explorer's included. Commit headroom below the reserve sheds at once, with no
debounce, and the victim is the session holding the most private commit
(`PagefileUsage` summed over its processes). There is no trend trip for commit: a JVM's
`-Xms` commits gigabytes in one step, which is fine while headroom remains. It freezes with
`NtSuspendProcess` over the tree and kills with `TerminateJobObject`. Peak memory is
resident memory, like on Linux and macOS. Windows keeps no peak working set for a job
(`PeakJobMemoryUsed` is committed memory, which would report a JVM with a large `-Xms` at
its full reservation), so syred samples the job's summed working sets on each watchdog
tick, and also takes any one process's own peak working set and the root's peak working
set, which stays readable after it exits because syred keeps a handle to it.

A process is assigned to its job right after it starts. A child spawned in the first
microseconds could escape; agent CLIs take far longer than that to start tools.

Some children are created outside the job anyway. Seen live: the Microsoft Store build
of PowerShell 7, which Claude Code uses as its shell on Windows, is inside the job, but
every process it starts is not, so a memory hog run from an agent's shell survived
`TerminateJobObject`. Their parent PID still names the process that started them, so
the guard walks descendants of the session's processes (about once a second while it
watches, and always before a shed, kill or release) and adopts any that escaped. It
checks creation times, so a reused PID can't pull in an unrelated process. An adopted
process goes into the job when Windows allows it. Otherwise syred holds a handle to it
and counts, freezes and kills it with the job.

Windows VMs are detected from the SMBIOS strings in
`HKLM\HARDWARE\DESCRIPTION\System\BIOS`. The CPUID hypervisor bit is not used, because it
is also set on bare-metal hosts running Hyper-V/VBS.

### macOS

No cgroups, so syred tracks each session's **process tree**. A process group alone is
not enough: Claude Code starts every shell command in a new process group (seen live), so
a memory hog run from an agent's shell sat outside the session's group and a group kill
missed it. The guard follows descendants by parent PID (about once a second while it
watches, and always before a shed or kill), checking start times against PID reuse. It
also remembers each process group a descendant starts, so a grandchild reparented to
launchd when its parent exits is still caught through its group. Freeze, kill and RSS
cover all of them.

The kernel's own pressure level,
`kern.memorystatus_vm_pressure_level`, is polled through sysctl. Syred builds with
`CGO_ENABLED=0`, so `DISPATCH_SOURCE_TYPE_MEMORYPRESSURE` is not available.

- **Normal**: full speed.
- **WARN, or an active `machine.throttle`**: every process in the tree gets
  `PRIO_DARWIN_BG` (what `taskpolicy -b` does: E-cores, throttled I/O). It is re-applied
  every tick so new children follow, and restored when pressure clears.
- **CRITICAL, available memory below the reserve, or swap growth past its budget while
  WARN or near the reserve** (see Watchdog): shed.

Available memory = total − (app + wired + compressed), the same as Activity Monitor. The
peak comes from the root's `wait4` rusage. A session's size (for victim choice and the
bystander check) is the tree's `phys_footprint` (`proc_pid_rusage`), which counts
compressed and swapped pages. Resident memory does not: live on bocamacmini, a hog
holding 10 GiB showed 921 MiB of RSS and was wrongly called a bystander.

## Control-plane priority

Under full load, syred and sshd must stay responsive. Unprivileged daemons can't raise
their own priority on Unix, so Syre **lowers the workers**. That is always allowed, and
children inherit it:

| | syred | workers |
|---|---|---|
| Linux | `oom_score_adj −500` best effort (needs CAP_SYS_RESOURCE) | `nice +5`, `oom_score_adj +300` (the kernel OOM killer prefers them) |
| macOS | `nice −10` best effort (needs root) | `nice +5` |
| Windows | above-normal priority class | normal; below normal while throttled |

Priorities apply in every mode except passthrough. `syred` logs which steps were refused.

## Watchdog

It runs only while a managed session exists and the mode isn't passthrough, so an idle
host pays nothing. It polls every 250 ms. It trips on:

1. available memory < reserve for more than 500 ms (at once under a container's limit),
2. kernel pressure level CRITICAL (macOS),
3. worker-pool PSI full avg10 ≥ 20% for 5 s (Linux scopes),
4. commit headroom < reserve (Windows), at once, or
5. swap growth past `min(25% RAM, 2 GiB)` since work began, for more than 500 ms, while
   the host is also short: pressure WARN or worse, or available memory under twice the
   reserve (macOS, and Linux without scopes, where no `MemorySwapMax` caps the workers).
   macOS pages a runaway out and keeps "available" high, so this is what catches it there.
   Growth alone is not enough: macOS rarely gives swap back, so a person's own apps can
   add gigabytes over a long session. After 2 minutes of calm (normal pressure, memory
   healthy) the baseline moves up to the swap in use then.

On a trip it picks the **lowest priority, then the largest** session (by committed
memory for a commit trip): ticket runs
first, then dispatched tasks, then a person's own chat, which goes only when nothing else
is left. Within a priority the session using the most memory goes, so a shed removes the
pressure instead of a small run that happened to start last; the most recently started
breaks a tie. Observed (unmanaged) sessions are never shed.

A victim holding under a quarter of the host's used memory is a **bystander**: the
machine was full for other reasons (a browser, a person's own chat). The evidence and the
summary say so.

A shed **freezes** the tree first, so allocation stops at once. It then records evidence
(the tree's RSS, the host's available memory, the reserve, how many sessions ran) and
tells the session's owner. Only then does it **kill**. The watchdog settles for 2 s before
it considers another shed.

## Outcome: requeue, not failure

A shed ends the run with `memory_pressure_shed`. A session whose root dies of SIGKILL
while the kernel's OOM-kill count (`/proc/vmstat oom_kill`, which counts a container's
OOM killer too) rose, and that the guard did not shed, is labeled `oom`: the chat error
kind and the digest's failure category and termination reason say so, and a verify
command's output says it was killed by the out-of-memory killer. That is a new error kind and failure
category, distinct from `oom`: `oom` is the job itself running out of memory, while a shed
means the host was full.

- **Ticket chats.** The chat manager reports the engine's (or the verify command's) exit
  as `memory_pressure_shed`, with evidence. The hub records the attempt as
  `requeued/memory_pressure_shed` and releases the ticket to **ready**. Shed attempts
  don't count against the self-repair retry budget.
- **Cooldown.** The machine gets no new ticket runs for 2 minutes, so it can recover
  first. Without it, a machine still short of memory sheds each new run within seconds
  and uses up the ticket's sheds. Each further shed doubles the cooldown, up to an hour,
  so a machine kept full by something outside Syre stops taking work instead of shedding
  a ticket every few minutes. A run that finishes on the machine, or an hour past its
  cooldown without a shed, ends the streak. Cooldowns live in the hub's memory and
  are rebuilt at startup by replaying the last two hours of attempts (sheds extend a
  streak at their end time, a completed run ends it), so a restart doesn't hand a
  still-short machine new work.
- **Auto-throttle.** The hub files a `machine.throttle` directive for 30 minutes, capping
  the machine at one fewer session than it was running. It is attributed to the
  guardrails and can be undone like any directive. It is not filed when it wouldn't
  tighten anything.
- **Escalation.** On the third shed of the same ticket, it goes to **needs you** instead:
  it may need more memory than these machines have. Only sheds the ticket caused count:
  a bystander shed is recorded as `requeued/host_memory_full` and never escalates.
- **Ad-hoc tasks** finish failed with category `memory_pressure_shed`. They are not
  requeued.

The digest carries `termination_reason` (`timeout | user_stop | memory_pressure_shed |
oom`), `peak_rss_mb` and `host_avail_ram_at_exit_mb`, and the summary reads like:

> Shed by host memory pressure: the machine dropped below its 410 MiB survival reserve,
> so Syre stopped the agent at 3.0 GiB. Not a code error; the work can run again.

so the Commander spends no tokens chasing a phantom code error.

## Policy

`machine.guardrails` is an ordinary directive: timed or open-ended, attributed, audited,
and pushed to the daemon with the others. `set_policy` takes it with
`constraints.guardrails` under the usual rule. A stricter mode
(`passthrough < adaptive < adaptive_strict`) applies at once with a receipt and undo. A
looser one returns `loosens` and needs `request_override`. The comparison is against the
machine's effective mode: the newest guardrails directive, else the base mode the daemon
reports.

`machine.throttle` now reaches the OS: lower CPU weight or background priority for
workers, and a hard ceiling for `cpu_ceiling_pct`.

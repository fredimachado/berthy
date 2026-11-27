# Health probes without tools in the image

## Problem

A Swarm role needs a health check for Berthy to gate a deploy. A minimal
image may have no shell or HTTP client. Issue #10 asks whether Berthy can
provide the executable. Issue #18 needs the same mechanism for workers.

## Questions and answers

The experiment used a single-node Swarm on Docker Engine 29.7.2 in WSL.
It used the isolated socket `unix:///var/tmp/berthy-phase2/docker.sock`, a
static Go probe, and a scratch-style image imported from a tar containing
only a static HTTP server. The image's health check was the exec form
`["CMD", "/.berthy/probe", "http"]`. The probe requested
`http://127.0.0.1:18080/health` and returned success on 2xx. Commands below
are excerpts from `spike10-run.sh`, run with
`MSYS_NO_PATHCONV=1 wsl -d Ubuntu -- bash /mnt/d/code/berthy-orchestrate/batch3/spike-10/spike10-run.sh`.
`d` in those excerpts is the specified Docker CLI with the isolated host.

1. **Mounting.** `d service create --detach --name spike10-bind --mount
   type=bind,src=/var/tmp/spike10-work/spike10-probe,dst=/.berthy/probe,readonly
   ... spike10-app:latest` created a healthy task. `d inspect` showed the
   exec-form health test and exit 0 with `old 200`. With the same command
   using the nonexistent source `spike10-absent`, `d service ps --no-trunc
   spike10-missing` showed a rejected task: `bind source path does not exist`.
   Swarm kept assigning replacement tasks. This tests one Engine version and
   one node, not all supported versions or a real multi-node deployment.

2. **Agent upgrades.** After `mv -f spike10-next spike10-probe` on the host,
   `d exec <old-task> /.berthy/probe http` still printed `old 200`. A new
   service with the same bind source became healthy and its probe printed
   `new 200`. The running task retains the old mounted file; the new task
   gets the replacement. Updating the agent alone does not update old tasks.

3. **Permissions.** The bind service also used `--user 12345:12345
   --read-only --cap-drop ALL`, with no security option override. `d inspect`
   showed user `12345:12345`, `ReadonlyRootfs=true`, `CapDrop=["ALL"]`,
   and `SecurityOpt=null`. Its health check passed. This is Docker's default
   seccomp configuration on this daemon. The host binary was world-executable.

4. **Named volume.** `d volume create spike10-volume`, then `d run --rm
   --name spike10-seed` with the host probe and the volume mounted, copied the
   probe into the volume. A service with `--mount
   type=volume,src=spike10-volume,dst=/.berthy,readonly` became healthy and
   printed `new 200`. This avoids a file bind at task start, but a local
   volume is per node and can be empty on another node. It needs trusted
   seeding, versioning, cleanup, and upgrade coordination. The experiment did
   not test a shared volume driver. A volume is not more robust by itself.

5. **Plans and adoption.** `serviceSpec` builds the health check and the
   named-volume mount list separately (`internal/deploy/spec.go:74`,
   `internal/deploy/spec.go:113`). A normal plan's diff compares manifests,
   not generated Docker specs (`internal/deploy/controller.go:453`,
   `internal/deploy/diff.go:237`). An implicit probe mount would not appear
   as a volume change today. Add a deterministic health and mount explanation
   to the plan; include new health fields in the release diff. Adoption
   compares live and desired specs (`internal/deploy/planner.go:333`). It maps
   mounts to `volumes` and health commands to `health.test`
   (`internal/deploy/adopt.go:65`), but only iterates live fields, so a mount
   present only in the desired spec is omitted (`internal/deploy/adopt.go:87`).
   If a live mount list exists, its changed value can be shown as `volumes`.
   Make the new mount and health command explicit in adoption differences,
   including when absent from the live service. Existing host mounts are
   rejected by adoption's runtime safety check (`internal/deploy/adopt.go:14`);
   do not broadly exempt host mounts to accommodate this feature.

6. **Compose.** Defer a Berthy-specific form for Compose. Berthy writes the
   supplied Compose file (`internal/deploy/compose.go:173`), asks Compose to
   resolve and hash it (`internal/deploy/compose.go:636`), then runs Compose
   up (`internal/deploy/compose.go:923`). Health checks come from the Compose
   file or image; the verdict accepts a running service with no check or a
   healthy check (`internal/deploy/compose.go:993`). Injection would change
   the file, hashes, host-bind policy and adoption behavior. The need exists,
   but it deserves a separate design rather than silently rewriting Compose.

The overlay helper is an authenticated HTTP server, not an executable health
check (`internal/probehelper/probehelper.go:1`). Its HTTP and TCP work delegates
to `monitor.ProbeHTTP` and `monitor.ProbeTCP`
(`internal/probehelper/probehelper.go:202`). Those functions can be reused
behind a small command entry point, with their timeout and status semantics
checked against the new form. Do not mount the helper server as-is.

## Chosen mechanism

Use a small static, versioned probe executable installed on every eligible node
at a stable path. Bind it read-only into each opted-in task at a reserved path
and use Docker's exec-form health check. Fail preflight with an actionable
error when the file, architecture, permissions or version is wrong. A
single-node preflight cannot prove readiness on other nodes; either verify each
eligible node or constrain placement to nodes whose installation is verified.
Protect the host path from untrusted replacement, reserve the container target,
and define what happens if an image or user mount already occupies it. Keep
old probe versions available for rollback and for tasks already running.

## How #18 uses it

Add `uptime` as a mutually exclusive health form. The probe reads process 1's
start time from `/proc/1/stat` and compares it with the monotonic boot uptime.
The experiment's `d exec <task> /.berthy/probe uptime 99999s` returned 1;
`uptime 0s` returned 0. It used 100 clock ticks per second, so production
must obtain or correctly establish the Linux clock tick rate and parse the
process name safely. Validate timing against deploy timeout, and set check
interval and start period so early failures do not exhaust retries before the
threshold. Test an actual short-lived worker and a surviving worker in Swarm;
the experiment did not test rollout timing or rollback.

## Decision

Build this in slices, subject to solving installation and verification on every
eligible node. First ship the probe and secure per-node installation with
versioned rollback and failure reporting. Next add HTTP health, deterministic
plans and adoption differences, validation, generated reference and Swarm e2e
tests. Add uptime in a separate slice with timing and failed-start tests. TCP
can follow when needed. Do not add Compose injection in these slices.

## Risks

A missing or incompatible binary prevents tasks from starting. A stale inode
keeps the old probe until a task is recreated. Node provisioning and rollback
must account for that behavior and for mixed agent versions. Custom seccomp,
user namespaces, mount policies, nonstandard `/proc`, different architectures,
and older Docker versions remain untested. Uptime proves only survival, not
that a worker is doing useful work. Protect the probe path because every task
executes its contents.

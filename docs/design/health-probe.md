# Health probes without tools in the image

## Problem

A Swarm role needs a health check for Berthy to gate a deploy. A minimal
image may have no shell or HTTP client. Issue #10 asks whether Berthy can
provide the executable. Issue #18 needs the same mechanism for workers.

## Questions and answers

The experiment used a single-node Swarm on Docker Engine 29.7.2 in WSL.
It used the isolated socket `unix:///var/tmp/berthy-phase2/docker.sock`, a
static Go probe, and a one-binary, shell-free image imported from a tar.
The image runs the experiment executable in `serve` mode. Its health check
was the exec form
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
   one node, not all supported versions or a real multi-node deployment. Engine
   25 is the supported minimum (`docs/spec-v1.md:329`); it was not available
   here and its file-bind and missing-source behavior remains to be tested.

2. **Agent upgrades.** After `mv -f spike10-next spike10-probe` on the host,
   `d exec <old-task> /.berthy/probe http` still printed `old 200`. A new
   service with the same bind source became healthy and its probe printed
   `new 200`. The running task retains the old mounted file; the new task
   gets the replacement. Updating the agent alone does not update old tasks.

3. **Permissions.** The bind service also used `--user 12345:12345
   --read-only --cap-drop ALL`, with no security option override. `d inspect`
   showed user `12345:12345`, `ReadonlyRootfs=true`, `CapDrop=["ALL"]`,
   and `SecurityOpt=null`. Its health check passed with no service security
   override. The experiment did not establish the applied seccomp profile.
   The host binary was world-executable.

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
   do not broadly exempt host mounts to accommodate this feature. The same
   `validateInheritedRuntime` check runs for restart, scale and exec
   (`internal/deploy/operations.go:133`, `internal/deploy/operations.go:678`).
   These operations must accept the exact expected read-only probe mount for
   an opted-in role. Test each operation and reject a different source, target
   or read-only flag.

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

On today's single server, bind the installed agent binary itself read-only,
for example `/usr/local/bin/berthy` to `/.berthy/berthy`. Use Docker's
exec-form health check, `["CMD", "/.berthy/berthy", "probe", "uptime", ...]`.
There is no separate probe installation. The `probe uptime` interface must
remain stable across agent versions: a task recreated from an older service
spec mounts the binary currently at the host path, not the old inode. Protect
the host path from untrusted replacement, reserve the container target, and
handle collisions with image paths or user mounts. Report an absent bind source
as a task rejection; a present but incompatible binary can let the app start
while its health check fails. Preflight should report detectable problems.
Version-specific bind sources and per-node verification belong to multi-server
work in #16.

## How #18 uses it

Add `uptime` as a mutually exclusive health form. The probe reads process 1's
start time from `/proc/1/stat` and compares it with the monotonic boot uptime.
The experiment's `d exec <task> /.berthy/probe uptime 99999s` returned 1;
`uptime 0s` returned 0. It used 100 clock ticks per second, so production
must obtain or correctly establish the Linux clock tick rate and parse the
process name safely. Validate timing against deploy timeout, and set check
interval and start period so early failures do not exhaust retries before the
threshold. `startFailed` counts only failed and rejected tasks
(`internal/deploy/rollout.go:224`), so a worker that exits 0 before the
threshold can repeatedly complete without consuming start attempts. Change
that accounting for this form. Test workers exiting both 0 and 1 before the
threshold; both must exhaust the configured start attempts and preserve the
previous release. Also test a surviving worker. The experiment did not test
rollout timing or rollback.

## Decision

Build on the single-server mechanism after establishing file-bind and
missing-source behavior on Engine 25, the supported minimum. First add the
stable agent probe interface and read-only mount, runtime safety checks,
plans and adoption differences. Add uptime with validation, generated reference,
start-attempt accounting and Swarm e2e tests. Add HTTP health and its tests as
a separate slice; TCP can follow when needed. Keep multi-server distribution
and version-specific sources in #16. Do not add Compose injection in these
slices.

## Risks

An absent bind source rejects the task. A present but incompatible binary
can allow the app to start while the health check fails and readiness is never
established. A running task keeps the old inode until recreated; a recreated
old spec gets the current binary, so the probe interface must remain compatible.
Custom seccomp, user namespaces, mount policies, nonstandard `/proc`, different
architectures and Engine 25 remain untested. Uptime proves only survival, not
that a worker is doing useful work. Protect the agent path because every
opted-in task executes its contents.

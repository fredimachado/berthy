# fakeserver

Disposable "fresh Linux servers" for testing `berthy setup`.

Each server is a privileged systemd container built from `debian:12`. It has
sshd, an `ops` user with passwordless `sudo`, a private address on a dedicated
bridge network, and **no Docker**, so `berthy setup`'s "install Docker with the
official script" step runs for real. Docker and Swarm run nested inside it.

## Requirements

- A Docker daemon that allows `--privileged` containers on cgroup v2. The script
  talks only to `unix:///var/tmp/berthy-phase2/docker.sock` (override with
  `FS_DOCKER_HOST`); it never touches the default daemon.
- Internet: Docker Hub (the base image, `alpine`, `nginx`) and
  `https://get.docker.com`.
- `bash`, `ssh`, `ssh-keygen` on the machine that runs the script (WSL here).
- The daemon must run on the same machine as the script: the containers'
  addresses are reached through that machine's bridge.

## Commands

```
./fakeserver.sh build                 # image berthy-fakeserver:debian12
./fakeserver.sh start NAME [IP]       # create/start; waits for systemd and sshd
./fakeserver.sh ip NAME
./fakeserver.sh ssh NAME CMD...       # ssh as ops through the fixture ssh config
./fakeserver.sh exec NAME CMD...      # docker exec, bypassing ssh
./fakeserver.sh install-docker NAME   # run get.docker.com inside, enable docker
./fakeserver.sh stop NAME
./fakeserver.sh rm NAME               # also removes the NAME-docker volume
./fakeserver.sh rm-all                # every labelled server, volume, and the network
```

`start` is idempotent. Without `IP` the lowest free address from `.10` of the
subnet is used (`172.31.250.0/24`, gateway `.1`, network `berthy-fakeservers`);
a restarted server keeps its address. Override with `FS_SUBNET`, `FS_GATEWAY`,
`FS_NETWORK`, `FS_IMAGE`.

Everything created is labelled `berthy.fakeserver=1`. `/var/lib/docker` and
`/var/lib/containerd` of each server are the named volumes `NAME-docker` and
`NAME-containerd`: overlayfs cannot sit on the container's own overlay
filesystem, and Docker 29 stores image layers under `/var/lib/containerd`
(with only `/var/lib/docker` on a volume, `docker run` fails with
`mount ... fstype: overlay ... invalid argument`). Nested images survive
`stop`/`start` and vanish on `rm`.

`get.docker.com` prints "WSL DETECTED" and waits 20 seconds when the host kernel
is WSL's (the container reports the host kernel). That is harmless.

## Pointing `berthy setup` at it

`start` creates a fixture home, `FS_HOME` (default `~/berthy-e2e/p5/home`):

```
FS_HOME/.ssh/id_ed25519          key with no passphrase, installed for ops and root
FS_HOME/.ssh/config              one "Host NAME" block per server (User ops, accept-new)
FS_HOME/.ssh/known_hosts
FS_HOME/bin/ssh, FS_HOME/bin/scp wrappers that add -F FS_HOME/.ssh/config
```

OpenSSH finds `~/.ssh/config` through the passwd entry, not `$HOME`, so setting
`HOME` alone does not select this config. The wrappers fix that: put them first
on `PATH` and `berthy setup`, which runs the operator's `ssh`, picks them up.

```
export FS_HOME=~/berthy-e2e/p5/home
export PATH="$FS_HOME/bin:$PATH" HOME="$FS_HOME"
berthy setup --host ops@s1 ...      # ssh resolves "s1" from the fixture config
ssh s1 -- sudo docker ps            # same config, by hand
```

A server that is recreated gets new host keys; `start` and `rm` drop the old
entries from `known_hosts` so `accept-new` does not trip.

Scenarios:

- fresh server: `build`, `start s1`, run setup.
- server with running apps: `start s2`, `install-docker s2`, create a Swarm and
  a service over `ssh s2 sudo docker ...`, then run setup.
- second run changes nothing: run setup twice and compare.
- rebuilt agent keeps its state: `exec`/`ssh` to remove and reinstall the agent
  while keeping the saved key.
- the laptop reaches the agent: any process inside listening on the server's IP
  is reachable from the host running the script (the CLI's `--listen` path).

## Base image and notes

`FROM debian:12`. Pulled digest at the time of writing:
`sha256:268fb24c39bca92d326dd83ea3abebe4a3f7751c804ffef23bb6d857dd343ff8`
(multi-arch index for `docker.io/library/debian:12`). The tag is pinned to the
major release; rebuild to pick up security updates.

Masked units (everything else runs): `getty.target`, `getty@tty1`,
`console-getty`, `serial-getty@ttyS0` (no ttys); `systemd-remount-fs`,
`systemd-timesyncd` (the host owns mounts and the clock);
`sys-kernel-debug.mount`, `sys-kernel-tracing.mount`, `sys-kernel-config.mount`,
`dev-hugepages.mount` (host kernel filesystems). The system comes up `running`
with no failed units.

Extra unit `berthy-ssh-hostkeys.service` generates the SSH host keys on first
boot, so each server has its own keys instead of the ones from the image build.

## Known limits

- A container, not a VM: it shares the host kernel (modules, sysctls, time and
  kernel version are the host's). `kmod` is present but `modprobe` cannot load
  modules the host lacks.
- No real reboot: `reboot` stops the container; `start NAME` brings it back,
  with its disk and `/var/lib/docker` volume, but not a fresh kernel boot.
- `/run` and `/run/lock` are tmpfs, as on a real server; `/` is overlayfs.
- Privileged means the "server" can alter the host (iptables, kernel
  parameters). Run only trusted code in it.
- Swarm ports, VXLAN and the ingress network work inside, but only between
  nested daemons on the same fixture network; no multi-host realism beyond that.
- No IPv6 and no public address: `--listen` tests use the private address.

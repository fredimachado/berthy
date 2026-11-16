# A disposable "fresh Debian server": systemd as PID 1, sshd, a sudo user, no Docker.
# Run it privileged (see fakeserver.sh) so Docker + Swarm can run nested inside.
FROM debian:12

ENV container=docker \
    DEBIAN_FRONTEND=noninteractive

# iptables and kmod are what a nested Docker daemon needs; iproute2 gives `ip`;
# procps gives ps/sysctl; curl + ca-certificates run the get.docker.com script.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus openssh-server sudo iproute2 curl \
      ca-certificates procps kmod iptables \
 && rm -rf /var/lib/apt/lists/*

# Units that make no sense in a container (no ttys, no hardware, the host owns
# the clock and the mounts). Kept to the minimum that keeps the system "running".
RUN systemctl mask \
      getty.target getty@tty1.service console-getty.service serial-getty@ttyS0.service \
      systemd-remount-fs.service systemd-timesyncd.service \
      sys-kernel-debug.mount sys-kernel-tracing.mount sys-kernel-config.mount \
      dev-hugepages.mount

# Every container must have its own host keys, not the ones from the image build.
# ssh-keygen -A is a no-op when the keys exist, so this is safe on every boot.
RUN rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub \
 && printf '%s\n' \
      '[Unit]' \
      'Description=Generate SSH host keys for this server' \
      'Before=ssh.service' \
      'ConditionPathExists=!/etc/ssh/ssh_host_ed25519_key' \
      '[Service]' \
      'Type=oneshot' \
      'ExecStart=/usr/bin/ssh-keygen -A' \
      '[Install]' \
      'WantedBy=ssh.service' \
      > /etc/systemd/system/berthy-ssh-hostkeys.service \
 && systemctl enable ssh.service berthy-ssh-hostkeys.service

# Key-only SSH. The authorized keys are installed at start, never baked in.
RUN printf '%s\n' \
      'PermitRootLogin prohibit-password' \
      'PasswordAuthentication no' \
      'KbdInteractiveAuthentication no' \
      > /etc/ssh/sshd_config.d/10-berthy.conf \
 && mkdir -p /run/sshd

# The operator account berthy setup logs in as.
RUN useradd --create-home --shell /bin/bash ops \
 && printf 'ops ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/ops \
 && chmod 0440 /etc/sudoers.d/ops \
 && install -d -m 0700 -o ops -g ops /home/ops/.ssh \
 && install -d -m 0700 /root/.ssh

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]

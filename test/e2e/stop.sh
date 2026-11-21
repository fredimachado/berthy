#!/bin/bash
# stop.sh stops the agent and the fakes, and removes the e2e Swarm services, containers, volumes and registry. It leaves
# $E2E_ROOT in place, so the logs of the last run stay readable.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

purge_fixture || exit 1
echo "stopped: agent and fakes stopped, e2e services, containers, volumes and $REG_NAME removed"

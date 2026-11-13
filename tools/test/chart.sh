#!/bin/sh
# Checks the chart: helm lint --strict, a render with the defaults and with every option on, what must be refused, and (when kubeconform is there) the rendered objects against the Kubernetes schemas.
# usage: tools/test/chart.sh        (CHART_REQUIRE=1: a missing kubeconform is a failure)
set -eu
cd "$(dirname "$0")/../.."
C=charts/blobgate
S="--set rqliteUrl=http://rqlite:4001 --set secrets.tenants.acme=a-secret-of-at-least-16 --set secrets.adminToken=an-admin-token-of-16c --set secrets.sealKey=a-seal-key-of-16-chars"
fail() { echo "chart: $*" >&2; exit 1; }

# shellcheck disable=SC2086
helm lint --strict $C $S >/dev/null || fail "helm lint --strict failed"
# shellcheck disable=SC2086
helm template t $C $S > /tmp/blobgate-chart-default.yaml
# shellcheck disable=SC2086
helm template t $C $S --set config.metricsPort=9100 --set serviceMonitor.enabled=true --set networkPolicy.enabled=true --set config.reserveBytes=1000 \
  --set config.deepCheck=true --set backup.existingSecret=bk --set backup.restoreOnEmpty=true --set backup.seconds=3600 --set replicas=3 > /tmp/blobgate-chart-full.yaml
grep -q 'BLOBGATE_TENANTS' /tmp/blobgate-chart-default.yaml || fail "the tenants did not reach the environment"
grep -q 'tenants: "acme=' /tmp/blobgate-chart-default.yaml || fail "the Secret has no tenants"
helm template t $C --set rqliteUrl=http://r:4001 --set secrets.shards.001=a-secret-of-at-least-16 --set secrets.adminToken=an-admin-token-of-16c --set secrets.sealKey=a-seal-key-of-16-chars | grep -q 'tenants: "001=' || fail "secrets.shards (the old name) is not read"
grep -q 'kind: PodDisruptionBudget' /tmp/blobgate-chart-default.yaml || fail "no PodDisruptionBudget with 2 replicas"
grep -q 'BLOBGATE_RESTORE_ON_EMPTY' /tmp/blobgate-chart-full.yaml || fail "restoreOnEmpty did not reach the environment"
grep -q 'BLOBGATE_RESERVE_BYTES' /tmp/blobgate-chart-default.yaml && fail "a setting at its default was written into the environment"

# what must be refused
helm template t $C >/dev/null 2>&1 && fail "a render with no rqliteUrl was accepted"
helm template t $C --set rqliteUrl=http://r:4001 >/dev/null 2>&1 && fail "a render with no secrets was accepted"
helm template t $C --set rqliteUrl=http://r:4001 --set existingSecret=mine >/dev/null 2>&1 || fail "existingSecret was not enough"
helm template t $C --set rqliteUrl=http://r:4001 --set existingSecret=mine --set serviceMonitor.enabled=true >/dev/null 2>&1 && fail "a serviceMonitor with no metrics port was accepted"

if command -v kubeconform >/dev/null 2>&1; then
  kubeconform -strict -ignore-missing-schemas -summary /tmp/blobgate-chart-default.yaml /tmp/blobgate-chart-full.yaml || fail "kubeconform"
elif [ "${CHART_REQUIRE:-}" = 1 ]; then
  fail "kubeconform is not installed"
fi
echo "chart: ok"

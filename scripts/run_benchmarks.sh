#!/usr/bin/env bash
set -euo pipefail

TARGETS=(
  kubernetes-kubelet
  kubernetes-apiserver
  prometheus
  etcd
  terraform
  hugo
  consul
  vault
traefik
  minio
  gitea
  containerd
  nats-server
  caddy
)

CSV_OUTPUT="${CSV_OUTPUT:-results.csv}"
TIMEOUT="${TIMEOUT:-30m}"
VARIANTS="${VARIANTS:-}"
NUM_WORKERS="${NUM_WORKERS:-}"
DATASETS_ROOT="${DATASETS_ROOT:-./datasets}"

mkdir -p "$(dirname "$CSV_OUTPUT")"

failed=()

for target in "${TARGETS[@]}"; do
  echo "=== Running benchmark: $target ==="
  env_vars=(
    "BENCHMARK=$target"
    "CSV_OUTPUT=$CSV_OUTPUT"
    "DATASETS_ROOT=$DATASETS_ROOT"
  )
  [ -n "$VARIANTS" ] && env_vars+=("VARIANTS=$VARIANTS")
  [ -n "$NUM_WORKERS" ] && env_vars+=("NUM_WORKERS=$NUM_WORKERS")

  if env "${env_vars[@]}" go test -v -run TestRTABenchmark -timeout "$TIMEOUT"; then
    echo "=== $target: PASSED ==="
  else
    echo "=== $target: FAILED ==="
    failed+=("$target")
  fi
  echo ""
done

echo "=== Done ==="
if [ ${#failed[@]} -gt 0 ]; then
  echo "Failed targets: ${failed[*]}"
  exit 1
else
  echo "All targets passed."
fi

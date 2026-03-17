#!/bin/bash

# Clone all benchmark datasets
# Usage: ./scripts/clone_datasets.sh [datasets_dir]

DATASETS_DIR="${1:-./datasets}"

mkdir -p "$DATASETS_DIR"
pushd "$DATASETS_DIR"

clone_if_missing() {
    local name="$1"
    local repo="$2"
    local tag="$3"

    if [ -d "$name" ]; then
        echo "Skipping $name (already exists)"
    else
        echo "Cloning $name..."
        git clone --depth 1 --branch "$tag" "$repo" "$name"
    fi
}

# Kubernetes
clone_if_missing "kubernetes" "https://github.com/kubernetes/kubernetes.git" "v1.32.0"

# Prometheus
clone_if_missing "prometheus" "https://github.com/prometheus/prometheus.git" "v2.54.0"

# etcd
clone_if_missing "etcd" "https://github.com/etcd-io/etcd.git" "v3.5.16"

# Terraform
clone_if_missing "terraform" "https://github.com/hashicorp/terraform.git" "v1.9.0"

# Hugo
clone_if_missing "hugo" "https://github.com/gohugoio/hugo.git" "v0.131.0"

# Consul
clone_if_missing "consul" "https://github.com/hashicorp/consul.git" "v1.19.0"

# Vault
clone_if_missing "vault" "https://github.com/hashicorp/vault.git" "v1.17.0"

# Grafana
clone_if_missing "grafana" "https://github.com/grafana/grafana.git" "v11.1.0"

# Traefik
clone_if_missing "traefik" "https://github.com/traefik/traefik.git" "v3.1.0"

# Minio
clone_if_missing "minio" "https://github.com/minio/minio.git" "RELEASE.2024-07-16T23-46-41Z"

# Gitea
clone_if_missing "gitea" "https://github.com/go-gitea/gitea.git" "v1.22.0"

# containerd
clone_if_missing "containerd" "https://github.com/containerd/containerd.git" "v1.7.20"

# NATS Server
clone_if_missing "nats-server" "https://github.com/nats-io/nats-server.git" "v2.10.18"

# Caddy
clone_if_missing "caddy" "https://github.com/caddyserver/caddy.git" "v2.8.4"

# Syncthing
clone_if_missing "syncthing" "https://github.com/syncthing/syncthing.git" "v1.27.10"

popd > /dev/null 2>&1

echo ""
echo "Done. Available datasets at $(realpath $DATASETS_DIR):"
ls -1 "$DATASETS_DIR"

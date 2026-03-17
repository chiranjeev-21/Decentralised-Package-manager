#!/usr/bin/env bash
# scripts/configure-pm.sh
# Configures npm, pip, and cargo to route through the p2p-ci local proxy.
# Run once per developer machine or CI image.
#
# Usage:
#   ./scripts/configure-pm.sh [--revert]
#   ./scripts/configure-pm.sh --proxy-addr 127.0.0.1:9090  # non-default address

set -euo pipefail

PROXY_ADDR="${P2PCI_LISTEN:-127.0.0.1:7878}"
PROXY_BASE="http://${PROXY_ADDR}"
REVERT=false

for arg in "$@"; do
  case "$arg" in
    --revert) REVERT=true ;;
    --proxy-addr=*) PROXY_ADDR="${arg#*=}"; PROXY_BASE="http://${PROXY_ADDR}" ;;
  esac
done

# ─── npm / yarn / pnpm ─────────────────────────────────────────────────────
configure_npm() {
  if command -v npm &>/dev/null; then
    if $REVERT; then
      npm config delete registry 2>/dev/null || true
      echo "npm: registry restored to default"
    else
      npm config set registry "${PROXY_BASE}/https://registry.npmjs.org"
      echo "npm: registry → ${PROXY_BASE}/https://registry.npmjs.org"
    fi
  fi

  if command -v yarn &>/dev/null; then
    if $REVERT; then
      yarn config delete registry 2>/dev/null || true
      echo "yarn: registry restored to default"
    else
      yarn config set registry "${PROXY_BASE}/https://registry.npmjs.org"
      echo "yarn: registry → ${PROXY_BASE}/https://registry.npmjs.org"
    fi
  fi
}

# ─── pip ────────────────────────────────────────────────────────────────────
configure_pip() {
  PIP_CONF="${HOME}/.config/pip/pip.conf"
  mkdir -p "$(dirname "${PIP_CONF}")"

  if $REVERT; then
    sed -i '/index-url.*p2pci\|trusted-host.*localhost/d' "${PIP_CONF}" 2>/dev/null || true
    echo "pip: index-url restored"
  else
    cat >> "${PIP_CONF}" <<EOF

[global]
index-url = ${PROXY_BASE}/https://pypi.org/simple
trusted-host = localhost
EOF
    echo "pip: index-url → ${PROXY_BASE}/https://pypi.org/simple"
  fi
}

# ─── cargo ──────────────────────────────────────────────────────────────────
configure_cargo() {
  CARGO_CONF="${HOME}/.cargo/config.toml"
  mkdir -p "$(dirname "${CARGO_CONF}")"

  if $REVERT; then
    sed -i '/p2pci\|replace-with.*p2pci/d' "${CARGO_CONF}" 2>/dev/null || true
    echo "cargo: source restored"
  else
    cat >> "${CARGO_CONF}" <<EOF

[source.crates-io]
replace-with = "p2pci"

[source.p2pci]
registry = "${PROXY_BASE}/https://index.crates.io"
EOF
    echo "cargo: source.crates-io → ${PROXY_BASE}/https://index.crates.io"
  fi
}

echo ""
echo "Configuring package managers to use p2p-ci proxy at ${PROXY_BASE}"
echo "────────────────────────────────────────────────────────────────"
configure_npm
configure_pip
configure_cargo
echo "────────────────────────────────────────────────────────────────"
echo ""

if $REVERT; then
  echo "Done. Package managers restored to upstream registries."
else
  echo "Done. Start the proxy with: make run"
  echo ""
  echo "Health check: curl ${PROXY_BASE}/_p2pci/health"
  echo "To revert:    ./scripts/configure-pm.sh --revert"
fi

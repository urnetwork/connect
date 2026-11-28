#!/usr/bin/env bash
# capture.sh -- Layer B of the fingerprint-drift harness (fingerprint/README.md).
#
# A thin wrapper over the Go capture tool that adds the one layer Go cannot do
# hermetically: the TCP SYN / IP TTL (JA4T), whose ground truth is the OS kernel
# and which needs root (tcpdump / AF_PACKET). Run it on LINUX with Docker; the
# SYN step needs sudo.
#
# It does NOT run in CI and the harness tests never invoke it. Agents must not
# run it against a shared stack.
#
# Usage:
#   ./capture.sh --chrome-version 141                 # TLS ClientHello golden + diff
#   ./capture.sh --chrome-version 141 --quic          # also the QUIC Initial golden
#   sudo ./capture.sh --chrome-version 141 --syn      # also the Linux SYN/TTL profile
#   ./capture.sh --synthetic                          # regenerate the synthetic golden (no Docker)
#
# Flags not listed here pass through to the Go tool (-chrome-image, -golden-dir,
# -timeout, ...).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
connect_dir="$(cd "${here}/../.." && pwd)"

syn=0
passthrough=()
for arg in "$@"; do
  case "${arg}" in
    --syn) syn=1 ;;
    *) passthrough+=("${arg}") ;;
  esac
done

cd "${connect_dir}"

# The TLS ClientHello and QUIC Initial goldens, captured at the endpoint by the
# Go tool (no root, no tcpdump).
go run -tags fingerprint_capture ./fingerprint/capture "${passthrough[@]}"

# The SYN/TTL layer: the Go tool cannot read the kernel's SYN, so capture it
# with tcpdump while a real dial crosses the wire. Docker-Chrome shares the host
# Linux kernel, so this is ONLY the Linux SYN profile; Windows/macOS/Android/iOS
# JA4T goldens must come from real-OS captures or a published p0f/JA4T database.
if [[ "${syn}" == "1" ]]; then
  if [[ "${EUID}" -ne 0 ]]; then
    echo "capture.sh: --syn needs root (tcpdump); re-run with sudo" >&2
    exit 1
  fi
  echo "capture.sh: --syn: capture the first SYN to the endpoint port with, e.g.:" >&2
  echo "  tcpdump -ni any 'tcp[tcpflags] & tcp-syn != 0 and tcp[tcpflags] & tcp-ack == 0' -c1 -w syn.pcap" >&2
  echo "then read its IP TTL and TCP options (MSS, window scale, SACK, timestamps) as the JA4T profile." >&2
  echo "This is the Linux profile only; see fingerprint/README.md for the non-Linux egress profiles." >&2
fi

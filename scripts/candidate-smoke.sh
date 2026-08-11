#!/usr/bin/env bash
set -euo pipefail

kind="${1:?smoke kind is required}"
software="${2:?software ID is required}"
image="${3:?immutable image reference is required}"
arch="${4:-amd64}"

case "$image" in
  *@sha256:[0-9a-f][0-9a-f]*) ;;
  *)
    if [ "${PCVM_SMOKE_ALLOW_TAG:-0}" != 1 ]; then
      echo "candidate smoke requires an immutable digest reference" >&2
      exit 2
    fi
    ;;
esac

data="$(mktemp -d "${RUNNER_TEMP:-/tmp}/pcvm-candidate-${software}.XXXXXX")"
name="pcvm-${software//[^a-zA-Z0-9_.-]/-}-${arch}"
chmod 0777 "$data"

cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  sudo chown -R "$(id -u):$(id -g)" "$data" >/dev/null 2>&1 || true
  rm -rf "$data"
}
trap cleanup EXIT

wait_ready() {
  local attempts="$1"
  for _ in $(seq 1 "$attempts"); do
    docker logs "$name" > "$data/container.log" 2>&1
    if grep -Fq '[PCVM] READY' "$data/container.log"; then
      return 0
    fi
    if [ "$(docker inspect -f '{{.State.Running}}' "$name")" != true ]; then
      docker logs "$name" >&2
      return 1
    fi
    sleep 1
  done
  docker logs "$name" >&2
  echo "${software} did not become ready" >&2
  return 1
}

base_run=(
  docker run -d --name "$name"
  --read-only --security-opt no-new-privileges --cap-drop ALL
  --tmpfs /tmp:rw,nosuid,nodev,size=512m
  --tmpfs /run:rw,nosuid,nodev,size=32m
  -v "$data:/home/container"
  -e CLEAR_CONSOLE=0
)

case "$kind" in
  minecraft)
    printf 'eula=true\n' > "$data/eula.txt"
    software_version=latest
    if [ "$software" = modrinth-modpack ]; then
      # Pin an immutable, server-tested project version. Modrinth's moving
      # latest for this project has previously shipped internally inconsistent
      # loader constraints, which should fail the pack rather than make the
      # PCVM release gate nondeterministic.
      software_version=EKFz1gv5
    fi
    minecraft_env=(
      -e "SOFTWARE=$software" -e SOFTWARE_VERSION="$software_version" -e SOFTWARE_BUILD=latest
      -e SERVER_PORT=25565 -e SERVER_MEMORY=3072M
    )
    if [ "$software" = modrinth-modpack ]; then
      minecraft_env+=(-e MODPACK_MODE=project -e MODPACK_PROJECT=sfs)
    fi
    "${base_run[@]}" --memory 4g --cpus 2 "${minecraft_env[@]}" "$image" >/dev/null
    wait_ready 1200
    docker stop --time 60 "$name" >/dev/null
    ;;
  app)
    "${base_run[@]}" --memory 2g --cpus 2 -p 0:18080 \
      -e "SOFTWARE=$software" -e SOURCE_MODE=upload -e SERVER_PORT=18080 \
      -e 'APP_READY_PATTERN=Hello World from PCVM!' "$image" >/dev/null
    wait_ready 600
    host_port="$(docker inspect -f '{{(index (index .NetworkSettings.Ports "18080/tcp") 0).HostPort}}' "$name")"
    curl --fail --silent --show-error "http://127.0.0.1:${host_port}/" | grep -Fq 'Hello World from PCVM!'
    docker stop --time 45 "$name" >/dev/null
    ;;
  game)
    "${base_run[@]}" --memory 2g --cpus 2 \
      -e "SOFTWARE=$software" -e SOFTWARE_VERSION=latest -e SOFTWARE_BUILD=latest \
      -e SERVER_PORT=27015 -e MAX_PLAYERS=4 -e SERVER_NAME='PCVM candidate smoke' \
      -e GAME_WORLD=pcvm-ci "$image" >/dev/null
    wait_ready 900
    docker stop --time 60 "$name" >/dev/null
    ;;
  vm)
    vm_cpus=2
    [ "$arch" != arm64 ] || vm_cpus=1
    "${base_run[@]}" -i --memory 3072m --cpus 2 \
      -e "SOFTWARE=$software" -e SOFTWARE_VERSION=3.24 -e SOFTWARE_BUILD=latest \
      -e VM_DISK_GB=2 -e VM_MEMORY_MB=1536 -e "VM_CPUS=$vm_cpus" "$image" >/dev/null
    wait_ready 1200
    printf '%s\n' \
      'uname -m | sed "s/^/PCVM_ARCH=/"' \
      'sudo -n id -u | sed "s/^/PCVM_ROOT=/"' > "$data/serial-input"
    timeout --signal=TERM --kill-after=5s 20s \
      docker attach --sig-proxy=false "$name" < "$data/serial-input" > "$data/serial.log" 2>&1 || true
    if [ "$arch" = arm64 ]; then machine=aarch64; else machine=x86_64; fi
    serial_ok=0
    for _ in $(seq 1 10); do
      docker logs "$name" > "$data/container.log" 2>&1
      if grep -Fq "PCVM_ARCH=$machine" "$data/container.log" && grep -Fq 'PCVM_ROOT=0' "$data/container.log"; then
        serial_ok=1
        break
      fi
      sleep 1
    done
    test "$serial_ok" = 1
    timeout --signal=TERM --kill-after=5s 120s docker stop --time 110 "$name" >/dev/null
    ;;
  *)
    echo "unsupported candidate smoke kind: $kind" >&2
    exit 2
    ;;
esac

test "$(docker inspect -f '{{.State.Running}}' "$name")" = false

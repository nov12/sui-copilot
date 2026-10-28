#!/usr/bin/env bash

set -u

if [ "$#" -lt 1 ]; then
  echo "usage: $0 BINARY [ARGS...]" >&2
  exit 2
fi

binary="$1"
shift

pid_file="${SUI_COPILOT_DEV_PID_FILE:-.task/sui-copilot.pid}"
pid_dir="$(dirname "$pid_file")"
lock_dir="${pid_file}.lock"
restart_delay="${SUI_COPILOT_DEV_RESTART_DELAY:-0.2}"
binary_name="$(basename "$binary")"
lock_held=0
server_pid=""

mkdir -p "$pid_dir"

is_sui_copilot_pid() {
  local pid="$1"
  local command_line

  if [ -r "/proc/$pid/cmdline" ]; then
    command_line="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)"
  else
    command_line="$(ps -p "$pid" -o command= 2>/dev/null || true)"
  fi
  [[ "$command_line" == *"$binary_name"* ]]
}

stop_pid() {
  local pid="$1"
  local attempt

  kill -TERM "$pid" 2>/dev/null || true
  for attempt in $(seq 1 50); do
    if ! kill -0 "$pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "[dev] sui-copilot did not exit in time; sending SIGKILL (pid=$pid)" >&2
    kill -KILL "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
}

stop_previous() {
  local previous_pid

  if [ ! -f "$pid_file" ]; then
    return
  fi

  previous_pid="$(sed -n '1p' "$pid_file" 2>/dev/null || true)"
  if [[ "$previous_pid" =~ ^[0-9]+$ ]] && kill -0 "$previous_pid" 2>/dev/null && is_sui_copilot_pid "$previous_pid"; then
    echo "[dev] stopping previous sui-copilot (pid=$previous_pid)"
    stop_pid "$previous_pid"
    sleep "$restart_delay"
  fi
  rm -f "$pid_file"
}

release_lock() {
  if [ "$lock_held" -eq 1 ]; then
    rm -f "$lock_dir/owner"
    rmdir "$lock_dir" 2>/dev/null || true
    lock_held=0
  fi
}

acquire_lock() {
  local attempt
  local owner_pid

  for attempt in $(seq 1 200); do
    if mkdir "$lock_dir" 2>/dev/null; then
      printf '%s\n' "$$" > "$lock_dir/owner"
      lock_held=1
      return
    fi

    owner_pid="$(sed -n '1p' "$lock_dir/owner" 2>/dev/null || true)"
    if [[ "$owner_pid" =~ ^[0-9]+$ ]] && ! kill -0 "$owner_pid" 2>/dev/null; then
      rm -f "$lock_dir/owner"
      rmdir "$lock_dir" 2>/dev/null || true
      continue
    fi
    sleep 0.05
  done

  echo "[dev] unable to acquire start lock: $lock_dir" >&2
  exit 1
}

cleanup() {
  if [ -n "$server_pid" ] && kill -0 "$server_pid" 2>/dev/null; then
    stop_pid "$server_pid"
  fi
  if [ -f "$pid_file" ] && [ "$(sed -n '1p' "$pid_file" 2>/dev/null || true)" = "$server_pid" ]; then
    rm -f "$pid_file"
  fi
  release_lock
}

handle_signal() {
  local exit_code="$1"

  cleanup
  trap - EXIT INT TERM
  exit "$exit_code"
}

trap 'handle_signal 130' INT
trap 'handle_signal 143' TERM
trap cleanup EXIT

acquire_lock
stop_previous

"$binary" "$@" &
server_pid=$!
printf '%s\n' "$server_pid" > "$pid_file"
release_lock

wait "$server_pid"

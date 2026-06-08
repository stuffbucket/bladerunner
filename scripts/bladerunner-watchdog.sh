#!/usr/bin/env bash
# bladerunner-watchdog.sh — guest-LOCAL wake-heal backstop.
#
# SINGLE SOURCE OF TRUTH: this file is checked in and --copy-in'd by the image
# build (scripts/build-guest-image.sh). The cloud-init path embeds a byte-for-
# byte copy of this body in internal/provision/cloudinit.go; keep them in sync.
#
# WHY THIS EXISTS: when the Mac sleeps, VZ pauses the guest vCPUs with no clean
# ACPI suspend and no paravirt "you were stopped" signal. On wake the guest has
# NO event telling it that it was suspended. A watchdog CANNOT detect "just woke"
# by comparing its own monotonic vs wall clock — both freeze and resume together
# (arch_sys_counter drives both; no kvm-clock, no /dev/ptp). So the only
# guest-local skew detector is an EXTERNAL reference: chrony's NTP offset (and
# the RTC, IF it tracks real time — UNKNOWN on VZ, so we only LOG it).
#
# CONFIRMED: stale vsock connectivity (no SSH banner) is the more certain
# symptom. PLAUSIBLE-but-UNCONFIRMED: post-sleep clock skew breaking OIDC JWTs
# (the wedged VM was restarted before its clock could be measured). This loop
# LOGS both signals EVERY cycle so the NEXT wedge is DIAGNOSED, not guessed.
#
# Heal is conservative: never blanket-restart systemd-networkd (it would disrupt
# running Incus container bridges); only bounce the stateless socat relays, and
# only bounce vsock-ssh under a tight gate (sshd up locally but the relay dead).
set -uo pipefail
[ -r /etc/default/bladerunner-watchdog ] && . /etc/default/bladerunner-watchdog
VSOCK_OIDC_LOCAL_PORT="${VSOCK_OIDC_LOCAL_PORT:-15556}"  # the guest TCP listener
TAG=bladerunner-watchdog

log() { logger -t "$TAG" -- "$*"; }

listening() { ss -tln | grep -q ":$1 "; }
unit_active() { systemctl is-active --quiet "$1"; }
unit_restarts() { systemctl show -p NRestarts --value "$1" 2>/dev/null; }

while :; do
  # --- OBSERVE: clock (chrony) -------------------------------------------
  # System time offset + Leap status are the external skew reference.
  tracking="$(chronyc tracking 2>/dev/null)"
  sys_offset="$(printf '%s\n' "$tracking" | awk -F': ' '/System time/ {print $2}')"
  leap="$(printf '%s\n' "$tracking" | awk -F': ' '/Leap status/ {print $2}')"
  refid="$(printf '%s\n' "$tracking" | awk -F': ' '/Reference ID/ {print $2}')"
  # --- OBSERVE: RTC-vs-realtime delta (the empirical test of the UNKNOWN:
  #     does the VZ RTC advance during host sleep? If yes, a future wedge
  #     will show a big spike here. We only LOG it; we do NOT heal from it
  #     until a real sleep confirms the RTC tracks real time.) ------------
  rtc_epoch="$(cat /sys/class/rtc/rtc0/since_epoch 2>/dev/null || echo NA)"
  now_epoch="$(date +%s)"
  if [ "$rtc_epoch" != NA ]; then rtc_delta=$((rtc_epoch - now_epoch)); else rtc_delta=NA; fi
  # --- OBSERVE: per-forwarder local health -------------------------------
  ssh_listen=$(listening 22 && echo up || echo down)
  api_listen=$(listening 8443 && echo up || echo down)
  oidc_listen=$(listening "$VSOCK_OIDC_LOCAL_PORT" && echo up || echo down)
  for u in bladerunner-vsock-ssh bladerunner-vsock-incus bladerunner-vsock-oidc bladerunner-vsock-ntp; do
    st=$(unit_active "$u.service" && echo active || echo inactive)
    log "fwd $u=$st nrestarts=$(unit_restarts "$u.service")"
  done
  log "clock sys_offset=${sys_offset:-NA} leap=${leap:-NA} refid=${refid:-NA} rtc_delta=${rtc_delta}s"
  log "listen ssh22=$ssh_listen api8443=$api_listen oidc${VSOCK_OIDC_LOCAL_PORT}=$oidc_listen"

  # --- HEAL: clock. If chrony reports not-synchronised, nudge it. Safe; a
  #     step does not disturb CLOCK_MONOTONIC, so timers/containers are fine.
  if [ "$leap" = "Not synchronised" ]; then
    log "heal: chronyc makestep (leap=Not synchronised)"
    chronyc makestep >/dev/null 2>&1 || true
  fi

  # --- HEAL: stateless socat relays (incus, then oidc). Restarting these
  #     only drops in-flight proxied connections; container/network state is
  #     owned by incusd + systemd-networkd, never by the relay. Bounce only
  #     when the relay is dead/inactive (its backend being down is the
  #     target's problem, not the relay's — do NOT bounce then).
  if [ "$api_listen" = up ] && ! unit_active bladerunner-vsock-incus.service; then
    log "heal: restart bladerunner-vsock-incus (backend :8443 up, relay inactive)"
    systemctl restart bladerunner-vsock-incus.service || true
  fi
  if ! unit_active bladerunner-vsock-oidc.service; then
    log "heal: restart bladerunner-vsock-oidc (relay inactive)"
    systemctl restart bladerunner-vsock-oidc.service || true
  fi
  if ! unit_active bladerunner-vsock-ntp.service; then
    log "heal: restart bladerunner-vsock-ntp (relay inactive)"
    systemctl restart bladerunner-vsock-ntp.service || true
  fi

  # --- HEAL: vsock-ssh LAST and tightly gated. Only when local sshd IS
  #     listening (:22 up, so the in-guest target is healthy) but the relay
  #     is dead. This avoids racing an operator's live 'runner shell' and
  #     avoids spinning the unit's unbounded ExecStartPre when sshd is down.
  #     The watchdog runs locally, so bouncing the bridge never cuts its own
  #     execution path. (Restart=always already auto-heals a crashed socat,
  #     so this fires only for the rare wedged-but-not-crashed case.)
  if [ "$ssh_listen" = up ] && ! unit_active bladerunner-vsock-ssh.service; then
    log "heal: restart bladerunner-vsock-ssh (sshd :22 up, relay inactive)"
    systemctl restart bladerunner-vsock-ssh.service || true
  fi

  # NEVER blanket-restart systemd-networkd: it would tear down Incus container
  # bridges. (A genuine total-network-down recovery is intentionally out of
  # scope until the journal data above proves it is needed.)
  #
  # OIDC probe honesty: ":$VSOCK_OIDC_LOCAL_PORT listening" proves only that the
  # guest-side socat TCP listener is up; it CANNOT confirm the host vsock peer
  # answers (the host is unreachable-by-design from a guest-local probe). The log
  # therefore distinguishes "relay dead" from "relay up, host unknown" — it does
  # NOT infer the host is fine.

  sleep 60
done

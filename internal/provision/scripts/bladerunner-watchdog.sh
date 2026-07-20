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
# guest-local skew detector is an EXTERNAL reference: chrony's NTP offset.
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
  # --- OBSERVE: per-relay local health -----------------------------------
  # All four channels run as instances of ONE template unit
  # (bladerunner-vsock-relay@<name>). Each entry is "<name> <gate-port>":
  # gate-port is a local backend whose listener must be up before a heal is
  # worthwhile (ssh :22, incus :8443); channels that dial OUT over vsock
  # (oidc, ntp) have no local backend, so gate-port is "-" (ungated).
  # The oidc gate-port is the guest-side TCP listener the relay itself opens,
  # tracked only for logging, not as a heal gate. ssh is listed LAST so it heals
  # after the others, preserving the original heal order.
  relays="incus 8443 oidc - ntp - ssh 22"
  ssh_listen=$(listening 22 && echo up || echo down)
  api_listen=$(listening 8443 && echo up || echo down)
  oidc_listen=$(listening "$VSOCK_OIDC_LOCAL_PORT" && echo up || echo down)
  # shellcheck disable=SC2086 # word-splitting "$relays" into name/gate pairs is intended
  set -- $relays
  while [ "$#" -ge 2 ]; do
    name=$1
    shift 2
    unit="bladerunner-vsock-relay@$name.service"
    st=$(unit_active "$unit" && echo active || echo inactive)
    log "fwd $name=$st nrestarts=$(unit_restarts "$unit")"
  done
  log "clock sys_offset=${sys_offset:-NA} leap=${leap:-NA} refid=${refid:-NA}"
  log "listen ssh22=$ssh_listen api8443=$api_listen oidc${VSOCK_OIDC_LOCAL_PORT}=$oidc_listen"

  # --- HEAL: clock. Don't wait for chrony's autonomous poll (up to maxpoll
  #     ~64s away) to notice a post-sleep offset: actively force a fresh host
  #     measurement NOW (burst). chrony.conf's 'makestep 1.0 -1' then steps the
  #     clock on that measurement for any offset >1s (and slews smaller ones),
  #     so re-sync is bounded by THIS loop's period, not loop + a chrony poll.
  #     A step never disturbs CLOCK_MONOTONIC, so timers/containers are fine.
  #     The explicit makestep is a backstop for when chrony has already parked
  #     the source as unreachable (its persistent makestep won't re-fire then).
  chronyc burst 4/4 >/dev/null 2>&1 || true
  if [ "$leap" = "Not synchronised" ]; then
    log "heal: chronyc makestep (leap=Not synchronised)"
    chronyc makestep >/dev/null 2>&1 || true
  fi

  # --- HEAL: the vsock relays, in ONE loop over the same channel table.
  #     Restarting a relay only drops in-flight proxied connections; container
  #     and network state is owned by incusd + systemd-networkd, never by a
  #     relay. Bounce a channel only when its instance is dead/inactive AND, for
  #     channels with a local backend, that backend IS up — its backend being
  #     down is the target's problem, not the relay's, so do NOT bounce then.
  #     (Restart=always already auto-heals a crashed socat, so this fires only
  #     for the rare wedged-but-not-crashed case.) A gate of "-" means the
  #     channel dials out over vsock and is healed whenever its instance is
  #     inactive. The ssh channel is gated on its local sshd :22 for the same
  #     reason as the others, which also avoids racing an operator's live
  #     'br shell' and spinning the instance's ExecStartPre when sshd is down.
  #     The watchdog runs locally, so bouncing a relay never cuts its own path.
  # shellcheck disable=SC2086 # word-splitting "$relays" into name/gate pairs is intended
  set -- $relays
  while [ "$#" -ge 2 ]; do
    name=$1 gate=$2
    shift 2
    unit="bladerunner-vsock-relay@$name.service"
    if unit_active "$unit"; then
      continue
    fi
    if [ "$gate" != "-" ] && ! listening "$gate"; then
      continue  # backend down: not the relay's fault, leave it be
    fi
    if [ "$gate" = "-" ]; then
      log "heal: restart $unit (relay inactive)"
    else
      log "heal: restart $unit (backend :$gate up, relay inactive)"
    fi
    systemctl restart "$unit" || true
  done

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

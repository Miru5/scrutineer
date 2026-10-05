#!/usr/bin/env bash
#
# Install this clone as a systemd *user* service.
#
# Scrutineer runs in the foreground for development and in the container image
# for everything else. This covers the third shape -- a long-lived instance
# built from a clone on a host you administer, which is how the members of a
# fleet sharing one PostgreSQL database are deployed. See docs/service.md for
# what the unit does and why; this script only writes it.
#
# A user unit, not a system one: the instance runs as whoever owns the clone,
# the data directory and the model token, and nothing it does needs root.
#
# Usage:   scripts/install-service.sh [-i NAME] [-n SERVICE] [-s DIR] [--now] [--dry-run]
# Example: scripts/install-service.sh -i sif --now
#
#   -i, --instance NAME   this instance's identity among those sharing a
#                         database. Required when scrutineer.yaml selects the
#                         postgres driver and does not set `instance:` itself.
#   -n, --name SERVICE    unit name, default "scrutineer". Change it to run a
#                         second instance from a second clone on one host.
#   -s, --skills DIR      skills directory, default "./skills" (relative to the
#                         clone, which is the unit's WorkingDirectory).
#       --now             enable and start it. Without this the unit is written
#                         and reloaded, and you start it yourself.
#       --dry-run         print the unit and the commands; change nothing.
#
# Idempotent: re-running overwrites the unit it wrote before. An existing unit
# with different contents is backed up next to it first, because a hand-edited
# one may hold local changes worth keeping -- and because a unit predating the
# config-only database selection carries an ExecStart that no longer starts
# (see docs/database.md).
#
set -euo pipefail

service=scrutineer
instance=
skills=./skills
start=false
dry_run=false

die() { printf 'install-service: %s\n' "$1" >&2; exit 1; }

while [ $# -gt 0 ]; do
    case "$1" in
        -i|--instance) instance=${2:?--instance needs a name}; shift 2 ;;
        -n|--name)     service=${2:?--name needs a unit name}; shift 2 ;;
        -s|--skills)   skills=${2:?--skills needs a directory}; shift 2 ;;
        --now)         start=true; shift ;;
        --dry-run)     dry_run=true; shift ;;
        -h|--help)     sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *)             die "unknown argument: $1 (try --help)" ;;
    esac
done

# The clone is the directory holding this script, not $PWD: the unit's
# WorkingDirectory has to be right whichever directory it was invoked from.
# shellcheck disable=SC1007  # CDPATH= is deliberate: a user CDPATH can redirect cd
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

case "$service" in
    *[!A-Za-z0-9_.@-]*|'') die "unit name must be a plain word: $service" ;;
esac

# --- preflight -----------------------------------------------------------
# Each of these fails the unit at startup rather than at install time, where
# the error lands in the journal instead of in front of whoever ran this.

[ -x "$root/bin/scrutineer" ] || die \
    "no binary at $root/bin/scrutineer -- build it first:
    cd $root && go build -o bin/scrutineer ./cmd/scrutineer"

[ -d "$root/$skills" ] || [ -d "$skills" ] || die \
    "skills directory not found: $skills (relative to $root)"

config=$root/scrutineer.yaml
if [ -f "$root/scrutineer.yml" ] && [ ! -f "$config" ]; then
    die "found scrutineer.yml, but the default config path is scrutineer.yaml -- rename it"
fi

# `instance` is mandatory on a shared database: two unnamed instances would
# each treat the other's scans as their own, so the binary refuses to start.
# Catch it here, where the message is visible.
if [ -f "$config" ] && grep -Eq '^[[:space:]]+driver:[[:space:]]*postgres' "$config"; then
    if [ -z "$instance" ] && ! grep -Eq '^instance:[[:space:]]*[^[:space:]]' "$config"; then
        die "scrutineer.yaml selects the postgres driver, so an instance name is required:
    pass --instance NAME, or add 'instance: NAME' to $config"
    fi
    [ -f "$HOME/.pgpass" ] || printf 'install-service: warning: no ~/.pgpass; the DSN carries no password\n' >&2
fi

# --- the unit ------------------------------------------------------------
# %h is systemd's specifier for the user's home, so every member's unit file
# is byte-identical apart from the identity that is meant to differ.
home_rel() { case "$1" in "$HOME"/*) printf '%%h/%s' "${1#"$HOME"/}" ;; *) printf '%s' "$1" ;; esac; }

exec_start="$(home_rel "$root/bin/scrutineer")"
[ -n "$instance" ] && exec_start="$exec_start -instance $instance"
exec_start="$exec_start -skills $skills"

unit=$(cat <<UNIT
[Unit]
Description=Scrutineer scanner${instance:+ ($instance)}
Documentation=https://github.com/alpha-omega-security/scrutineer
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# -skills is relative, and -config is absent because it defaults to
# ./scrutineer.yaml -- both resolve against this directory.
WorkingDirectory=$(home_rel "$root")
# Model token in a file rather than the unit, so it is not readable from
# \`systemctl cat\` or the journal. The leading '-' keeps the unit loadable
# before the file exists.
EnvironmentFile=-$(home_rel "$root")/.env
# The DSN carries no password; the driver reads it from here.
Environment=PGPASSFILE=$(home_rel "$HOME/.pgpass")
ExecStart=$exec_start
Restart=on-failure
RestartSec=5s
# A scan pipeline is long-running. SIGKILL part-way through leaves rows
# running with a fresh heartbeat that nothing clears for thirty minutes, so
# give in-flight work time to unwind instead.
TimeoutStopSec=120

[Install]
WantedBy=default.target
UNIT
)

unit_dir=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user
unit_path=$unit_dir/$service.service

if [ "$dry_run" = true ]; then
    printf '# would write %s\n\n%s\n\n# then:\n' "$unit_path" "$unit"
    printf 'systemctl --user daemon-reload\n'
    [ "$start" = true ] && printf 'systemctl --user enable --now %s\nloginctl enable-linger %s\n' "$service" "$USER"
    exit 0
fi

mkdir -p "$unit_dir"
if [ -f "$unit_path" ] && ! printf '%s\n' "$unit" | cmp -s - "$unit_path"; then
    backup=$unit_path.$(date +%Y%m%d%H%M%S).bak
    cp -p "$unit_path" "$backup"
    printf 'install-service: existing unit differed; kept a copy at %s\n' "$backup"
fi
printf '%s\n' "$unit" > "$unit_path"
printf 'install-service: wrote %s\n' "$unit_path"

systemctl --user daemon-reload

if [ "$start" = true ]; then
    systemctl --user enable --now "$service"
    # Without lingering, a user unit stops when the last session ends -- which
    # on a scanner VM nobody stays logged into means it stops almost at once.
    loginctl enable-linger "$USER" || printf 'install-service: warning: could not enable lingering; the service stops at logout\n' >&2
    printf 'install-service: started. Follow it with:  journalctl --user -u %s -f\n' "$service"
else
    printf 'install-service: start it with:  systemctl --user enable --now %s\n' "$service"
    printf '                 and keep it running when logged out:  loginctl enable-linger %s\n' "$USER"
fi

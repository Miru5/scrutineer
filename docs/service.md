# Running Scrutineer as a service

Scrutineer runs in the foreground for development, and upstream's answer for
anything longer-lived is the container image (see the README). This page covers
the third shape: a long-running instance built from a clone on a host you
administer, which is how the members of a fleet sharing one PostgreSQL database
are deployed.

It is written for systemd. Nothing here is required by the binary — Scrutineer
handles `SIGTERM` and exits cleanly whatever starts it — but a fleet member has
a few obligations that are easy to get wrong by hand, and they are collected
below.

## The unit

A **user** unit, not a system one: the instance runs as the person who owns the
clone, the data directory and the model token, and nothing it does needs root.

```ini
# ~/.config/systemd/user/scrutineer.service
[Unit]
Description=Scrutineer scanner
Documentation=https://github.com/alpha-omega-security/scrutineer
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%h/scrutineer
# Model token in a file rather than the unit, so it is not readable from
# `systemctl cat` or the journal. The leading '-' keeps the unit loadable
# before the file exists.
EnvironmentFile=-%h/scrutineer/.env
# PostgreSQL password, kept out of the DSN (and so out of `ps`). Only needed
# on a shared database; harmless otherwise.
Environment=PGPASSFILE=%h/.pgpass
ExecStart=%h/scrutineer/bin/scrutineer -instance <name> -skills ./skills
Restart=on-failure
RestartSec=5s
# A scan pipeline is long-running. Give in-flight work time to unwind instead
# of orphaning containers — see Stopping below.
TimeoutStopSec=120

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now scrutineer
loginctl enable-linger "$USER"   # keep it running when nobody is logged in
```

`%h` is systemd's specifier for the user's home, which keeps every member's
unit byte-identical: a `systemctl cat` diff between two hosts then shows only
the identity that is genuinely meant to differ.

### Why WorkingDirectory matters

Two of the arguments above are relative to it, and one argument is missing
because of it:

- `-skills ./skills` resolves against the working directory.
- `-config` is not passed at all: it defaults to `./scrutineer.yaml`.

Set `WorkingDirectory` to the clone and both work. Point it elsewhere and the
instance starts with the bundled skills and no configuration, which on a shared
database means it will not start at all (see below).

## Configuration, not flags

The database backend is **config-only**, with no flag of its own. An instance
on a shared PostgreSQL needs:

```yaml
# ~/scrutineer/scrutineer.yaml
database:
  driver: postgres
  dsn: postgres://scrutineer@db.example:5432/scrutineer?sslmode=verify-full&sslrootcert=/path/to/ca.crt
instance: <name>
```

`instance` may equally be given as `-instance <name>` on the command line,
which overrides the file. It is mandatory with `driver: postgres`: the binary
refuses to start without it, because two unnamed instances would each treat the
other's scans as their own. See [fleet.md](fleet.md).

Two notes on the DSN:

- Write `sslrootcert` as an absolute path. Neither libpq nor the Go driver
  expands `~` in it. Dropping the CA at `~/.postgresql/root.crt` instead lets
  you omit the parameter entirely.
- Leave `require_auth` out. It is a libpq parameter — fine in a `psql` or
  psycopg connection string — but the Go driver does not recognise it and
  rejects unknown keywords rather than ignoring them, so the instance will not
  connect.

## Credentials

| secret | where | why |
|---|---|---|
| model token (`CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`) | `~/scrutineer/.env`, loaded by `EnvironmentFile=` | keeps it out of `systemctl cat` and the journal |
| PostgreSQL password | `~/.pgpass`, mode 0600 | keeps it out of the DSN, out of `ps`, and out of the config file |
| CA certificate | wherever the DSN's `sslrootcert` points, world-readable | a certificate, not a secret; `sslmode=verify-full` needs it |

## Restarting

A restart is not free on a fleet member, and the cost is worth knowing before
you put it behind `Restart=on-failure`.

On startup the instance sweeps **its own** interrupted scans — rows it left
`running` — and fails them with "server restarted during run". The automatic
retry pass then re-enqueues them: twice by default, the first after five
minutes, doubling. So a restart loses no work permanently, but a *crash loop*
spends that budget on scans that never had a chance to run. If the unit is
restarting repeatedly, stop it rather than letting it cycle:

```sh
systemctl --user stop scrutineer
journalctl --user -u scrutineer -n 200 --no-pager
```

`-auto-retry-max 0` disables the pass entirely if you want a restart to leave
failures alone.

## Stopping

`TimeoutStopSec` is load-bearing. On `SIGTERM` the HTTP server drains within
five seconds, but the worker has to unwind whatever containers are running. A
`SIGKILL` part-way through leaves rows `running` with a fresh heartbeat, and on
a shared database nothing clears those until the heartbeat goes stale thirty
minutes later — the reaper cannot distinguish them from a scan still working on
another host. Two minutes of grace is enough for the usual case.

Before a planned stop of any length, consider draining instead: the queue is
partitioned per instance, so work queued here waits for *this* instance and no
other member will pick it up.

## Upgrading

```sh
cd ~/scrutineer
git fetch … && git checkout …          # or fetch from a bundle
go build -o bin/scrutineer ./cmd/scrutineer
systemctl --user restart scrutineer
```

Rebuild before restarting, not after: the unit starts the binary at
`bin/scrutineer`, so a half-finished build is what comes back up.

Schema migrations run automatically on startup and only ever add columns. On a
shared database the first instance to start after an upgrade performs them for
everyone, and members still running the old binary are unaffected — but see
[fleet.md](fleet.md) for the two things that are *not* backward compatible
across a mixed-version fleet: queue partition names and the abandoned-scan
reaper.

## Logs

```sh
journalctl --user -u scrutineer -f
journalctl --user -u scrutineer --since -1h --no-pager
```

The instance logs to stderr, so everything lands in the journal. Nothing is
written to a log file in the data directory.

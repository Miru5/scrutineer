# Running several instances on one database

A single Scrutineer instance owns its database. The PostgreSQL backend lets
several of them point at the same one, which is what this layer
(`internal/fleet`) is for: every member sees the whole picture — which
repositories exist, what has been scanned, every finding — while running only
their own work, on their own host, under their own model account.

It is opt-in and inert until you name an instance. With no name, Scrutineer
behaves exactly as it does today on SQLite.

## Starting an instance

Point each one at the shared database and give it a name:

In `scrutineer.yaml`, beside the database block the backend is selected with
(see [database.md](database.md)):

```yaml
database:
  driver: postgres
  dsn: postgres://scrutineer@db.example:5432/scrutineer?sslmode=verify-full
instance: miruna
```

The name may also be given as `-instance miruna` on the command line, which
overrides the config. The backend itself is config-only — there is no flag for
it.

The name is an identity, not a label: it is stamped on every scan the instance
creates and it names that instance's queue partition. Pick something stable and
short — the person or host it runs on — and do not change it afterwards
(see Renaming below). A PostgreSQL backend without an instance name is refused
at startup, because two unnamed instances would each treat the other's scans
as their own.

Nothing else differs between members. They run the same binary and the same
skills, and each keeps its own `-data` directory, model token and concurrency.

For keeping an instance running across reboots, and what a restart costs a
fleet member, see [service.md](service.md).

## What is shared and what is yours

The rule is **reads are fleet-wide, writes are scoped**: seeing that a teammate
already scanned a repository is the point of sharing a database; starting,
stopping or requeueing their work is not. Only the runner that owns a scan
holds its container and its queue message, and an enqueue on your instance runs
under your model account.

| | Scope |
|---|---|
| Repositories, findings, advisories, packages, maintainers | Shared — every instance sees all of them |
| Scan listings, repository listings, search | Shared, with an Instance filter and column |
| Retry, cancel, pause, resume, "retry all failed" | Your own scans only |
| The startup sweep of interrupted scans | Your own scans only |
| The job queue | One partition per instance — a runner only dequeues what its own UI enqueued |

Acting on a teammate's scan answers `409 Conflict` naming the instance that
owns it, rather than failing quietly.

Repositories themselves belong to nobody: any instance may add, scan or rescan
any of them. Two members scanning the same repository at the same time is
allowed — coordinate it between yourselves, or let the finding deduplication
sort out the overlap.

## Upgrading an existing deployment

Pointing an existing single-instance deployment at a shared database needs no
preparation. On first start the layer takes ownership of the rows written
before it existed — both the NULLs `AutoMigrate` leaves behind when it adds the
column and the empty names an unnamed deployment wrote — so they read as that
instance's own, and everything scoped (the sweep, retry-all, cancel-all-queued,
the queued count, the single-scan actions) keeps covering its full history.

The first named instance to start is the one that adopts them, which is what
upgrading means in practice: everything unowned was written by the single
deployment that existed. A member joining afterwards finds nothing unowned
left and takes none of it. If you are moving several deployments onto one
database at once, start them one at a time, or move the rows yourself with the
`UPDATE` below before the second one starts.

## Retiring or renaming an instance

Drain before retiring: nothing consumes a retired instance's queue partition, so
work still queued there waits forever. Cancel or let its queue empty first.

Renaming is the same problem in a different shape — the old name's scans stay
owned by a name nothing runs under. Prefer keeping the name; if you must change
it, move the rows with it:

```sql
UPDATE scans SET instance = 'new' WHERE instance = 'old';
```

Queued rows additionally hold a message in the old partition, so rename only
when nothing is queued or running.

## Throughput

Each instance runs its own workers against its own partition, so a member with
an empty queue does not help a member with a backlog — concurrency is the knob
that matters per instance, not the number of instances. Set `concurrency:` on
each host for that host (the work is API-bound: containers spend their time
waiting on the model), and watch per-account usage limits, which are also per
instance.

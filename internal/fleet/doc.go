// Package fleet lets several independent Scrutineer instances share one
// PostgreSQL database.
//
// It is a layer on top of the PostgreSQL backend, not part of it: the backend
// makes one database reachable from several processes, and this package makes
// those processes behave like separate owners of their own work inside it.
// Nothing here is needed on SQLite, and with no instance name configured every
// function degrades to the single-instance behaviour it replaces.
//
// # The rule
//
// Reads are fleet-wide, writes are scoped. Seeing that a teammate already
// scanned a repository is the point of a shared database; starting, stopping
// or requeueing their work is not. Only the runner that owns a scan holds its
// container and its queue message, and an enqueue here runs under this
// member's model account — so every mutation is restricted to this instance's
// own rows, while every list stays whole.
//
// # What belongs to whom
//
//   - A scan belongs to the instance that created it (Scan.Instance, stamped
//     by the callback Install registers). SweepRunning, the retry/pause/cancel
//     paths and the automatic retry pass are scoped to it.
//   - The job queue is partitioned by instance name (QueueName), so a runner
//     only ever dequeues work its own web UI enqueued.
//
// # Integration surface
//
// Everything this layer needs from the rest of the tree is a call into this
// package, so `grep -rn "fleet\." --include=*.go` lists every site. The few
// places where the seam is not a call carry a "fleet:" comment instead:
//
//   - the model field db.Scan.Instance;
//   - the queue name parameter on queue.New, which fleet.QueueName supplies;
//   - the Instance filter and column in the scans and repositories templates,
//     each gated on whether any scan carries an instance at all.
//
// `grep -rn "fleet[.:]"` therefore prints the complete surface, which is what
// makes removing or rebasing the layer mechanical.
//
// # Startup
//
// cmd/scrutineer calls fleet.Install once, after db.Open and before anything
// creates a scan. Install registers the owner stamp, backfills rows written
// before this layer existed, and records the name the rest of the package
// reads. Everything else follows from that one call.
package fleet

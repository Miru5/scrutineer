package fleet

// QueueName partitions the job queue by instance.
//
// goqite hands each message to exactly one consumer, so with a single "scans"
// queue any instance could dequeue any member's job and run it on their host
// under their model account. Naming the queue per instance means a runner only
// ever sees the work its own web UI enqueued, which is what keeps one member's
// usage limits, paused queue or outage from touching anyone else. With no
// instance name the base name is returned unchanged, so an existing
// deployment's queued rows are still found.
func QueueName(base string) string {
	if instance == "" {
		return base
	}
	return base + "-" + instance
}

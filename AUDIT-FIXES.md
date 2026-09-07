# ENG-2345: durable runner lifecycle

The production entry point now requires the named Firestore database configured by this module. The old handler methods remain for compatibility tests; Terraform uses the authenticated durable handlers.

| Audit finding | Change |
| --- | --- |
| 1: duplicate JIT and recycled names | A transactional job lease serializes workers across instances. Each generation gets a random name. Persisted JIT, template, zone, machine and Compute request ID survive callback retries. Zonal operations resolve uncertain insert outcomes before releasing reservations. |
| 2: canceled and stale demand | Completion is monotonic, including unassigned/group-zero jobs. Workers read current GitHub job status before admission; stale recreate capabilities cannot create a new generation directly. |
| 3: detached cleanup | Scheduler invokes synchronous, bounded orphan sweeps every two minutes, independent of webhook traffic. |
| 4: unbounded ingress | Authenticate source/signature format before reading, limit request bodies to 1 MiB, and set HTTP timeouts. Log event identifiers instead of webhook bodies. |
| 5: cost and starvation | Transactional fleet and on-demand admission limits, a machine override allowlist, and separate create/delete/maintenance queues. Create rate and concurrency are independently configurable. |
| 6: case-sensitive labels | Match label groups and magic machine labels case-insensitively. |
| 7: callback authorization | Workers require verified Google OIDC for the dedicated callback identity and fixed audience. VM recreation uses expiring, purpose-scoped, generation-bound capabilities. Callback URLs come from configuration. |
| 8: client churn | Reuse Compute, Tasks, Secret Manager and HTTP clients; remove the unused Secret Manager client from JIT generation. |
| 9: image pipeline | Implemented by the companion spock-runner PR: selective builds, weekly refresh, exact image selection and conservative retention. |
| 10: idle gate suppresses demand | Track each queued job separately; no fleet-wide idle boolean. |
| 11: lost origin/top-up | Keep origin and deletion intent in Firestore through VM deletion and enqueue/API failures. Reconciliation retries them independently. |

Discovery pages visible organization repositories and unfinished workflow runs every five minutes, recovering queued jobs even if their webhook was lost. Each page is a bounded retryable task. For enterprise installations, supply `discovery_repositories`; organization installations can also use this list to bound API usage. The PAT needs Actions read permission on every participating repository, in addition to runner registration permission. A GitHub 404 fails closed because it can indicate missing access.

## Operations and rollout

- Spock intentionally tracks this repository’s default branch, `master`, and its `master` image tag. The next Spock deployment resolves the updated module and image digest without a Spock PR. Publish the image before deploying the consumer; separate module and image lookups are not an atomic release. Immutable commit tags remain available for rollback.
- Drain existing runner generations before migration, or explicitly budget for them. The new admission counters cover durable reservations, not VMs created by older releases. Existing HMAC worker callbacks are rejected after migration; scheduled discovery reconstructs queued demand. Running old VMs retain their runtime limit and are swept when stopped.
- Completed records expire after seven days only after reservations and pending deletion have cleared. Active demand has no TTL. The named database is abandoned rather than deleted on Terraform destroy. Reimport it before recreating infrastructure; never reset counters independently of reservations.
- Fleet saturation is acknowledged and deferred; transient API failures retain one bounded Cloud Tasks retry chain. Durable reconciliation outlives that chain. Operators should check queue age, lifecycle errors, stopped VMs and reservations before changing limits.
- Cloud Run scales to zero when idle (`min_instance_count=0`, `max_instance_count=3`). Reconciliation and sweeping wake it every two minutes, discovery every five minutes, and an invariant audit hourly. No correctness requirement depends on a warm process. Scheduled requests, storage and active processing still incur usage; production savings have not been measured.
- Monitoring includes creation/JIT/rate/zone failures, deletion/sweep/discovery errors, and queued records older than 15 minutes. Supply notification channels in the consuming repo.

## Validation

Run `go test -race ./pkg/... ./test/... -skip '^(TestCreateCallbackTask|TestGenerateRunnerJitConfig|TestDeleteNotExistingVM)$'` from `runner-autoscaler`. The skipped cases require live credentials and can create tasks or registrations. Regression cases cover concurrent workers, cancellation/reordering, fleet admission, API outages, expired leases, ambiguous inserts, JIT refresh, pending deletion, request bounds and callback scope.

Terraform validation uses a consuming-root fixture with a local module source and dummy Google provider settings; it does not plan or apply live infrastructure. CI also runs actual Firestore emulator transactions for capacity transfer, version guards, due pagination and job-only writes. Before deployment, validate real IAM/OIDC and Scheduler delivery in staging, then observe a full create/run/delete cycle. Emulator tests cannot establish production IAM correctness or throughput.


## Consumer configuration requirements

Terraform 1.9 or newer is required for cross-variable validation. Non-preemptible fleets require on-demand provisioning and a positive on-demand admission limit; both Terraform and application startup reject an impossible configuration. `firestore_location` can override the runtime region with a supported Firestore region or multi-region. The default remains the runtime region for compatibility with this rollout; consult [Firestore locations](https://cloud.google.com/firestore/native/docs/locations) before deploying outside it. An existing database location cannot be changed in place.

## Demand, capacity and reconciliation contract

`jobs/<key>` contains durable demand and an optional claim on a runner generation. `runners/<generation>` is the capacity ledger, including generations detached from their originating job. A claim is not a GitHub assignment. Workers read current job status and check the runner's observed GitHub `busy` state before treating it as available. The GitHub runner ID from JIT registration is retained, making this a direct GET rather than repeatedly scanning all registrations.

If A's runner RA is busy with B, A relinquishes its claim while RA stays counted in the ledger. B relinquishes its unused RB when GitHub reports B in progress. An idle detached RB can then be claimed transactionally by A without adding capacity. If RB was preempted, A creates replacement capacity on a subsequent due pass while RA continues serving B. Available capacity is shared conservatively within the same source, repository and case-normalized label set; no repository-eligibility assumption is made across pools. Completion deletes the actual reported generation. Detached generations are released idempotently after confirmed deletion/absence, never merely because a job relinquishes a claim.

The hot query uses the explicit indexed pair `NeedsReconcile=true` and `NextActionAt<=now`, ordered by due time and document ID. Settled terminal tombstones retain their seven-day TTL but never enter that query. Terminal records with pending deletion or an unreleased claim remain actionable. Queued demand normally checks after two minutes; in-progress demand and healthy detached runners check after ten minutes, with completion webhooks and stopped-VM sweeps accelerating cleanup. No full tombstone traversal or terminal-only filter is used.

`EnqueueToken` and `EnqueuedUntil` cover one bounded retry chain. The marker is written before enqueue; a crash or ambiguous enqueue result is recovered after marker expiry. Workers reject obsolete dispatch tokens. Lease contention is acknowledged, fleet saturation schedules a later attempt, and permanent configuration failures are persisted and alerted instead of hot-retried. After correcting a parked configuration failure, an operator must clear that job's `Failure` and set its next action due. Active reservations still reconcile for cleanup. Only reservation/model transitions read and update `control/fleet`; ordinary webhook, lease, scheduling and status writes do not read the global counter.

GitHub primary/secondary rate responses honor `Retry-After` and reset time through a persisted PAT-wide not-before gate. Permission errors (including masked job-status 404s) retain demand and pause that repository for fifteen minutes. Discovery acknowledges deferred pages and the next scheduled traversal reconstructs them after the gate opens. Network/5xx failures use the bounded exponential Cloud Tasks retry policy. The backoff gates survive process termination and scale-to-zero.

## Invariant audit and state compatibility

The hourly authenticated audit reads a consistent runner-ledger/counter snapshot, inventories matching GCE generations, and checks the counter revision again. If capacity changed during the external inventory it defers rather than report a transient inconsistency. It alerts on high/low counter drift, unexpected GCE generations, provisioning-model mismatches and confirmed missing VMs. Pending/ambiguous inserts legitimately count even when no VM is visible. The audit scans live reservations, not job tombstones.

Repair is deliberately operator-controlled: pause all lifecycle queues and schedulers (including deletion and sweeping), quiesce webhook writers, and wait for active requests, leases, and Compute operations to settle. Reconcile the ledger against actual generations and rebuild both counters from the settled ledger before resuming. Resolve unmanaged generations and ambiguous inserts first. Never overwrite the counter with the instantaneous GCE VM count or repair while lifecycle writers are active. This PR supplies discrepancy detection and alerting, not an automatic repair endpoint that could breach the fleet ceiling under concurrent inserts.

Schema version 1 is checked at startup (`control/schema`) and on each job/runner read used for mutation. Unknown versions fail closed and emit initialization/lifecycle alerts. A database with pre-versioned job records is rejected rather than silently upgraded. This is a pre-deployment contract: if any earlier PR build was deployed, drain it and perform an explicit state migration before this version. Future semantic changes must increment the version and ship a migration; rollback across versions requires restoring/migrating a compatible database. Tracking `master` does not waive this contract.

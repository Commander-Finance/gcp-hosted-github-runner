# Durable runner lifecycle

The autoscaler keeps all demand and capacity state in a dedicated Firestore database named `github-runners`. Webhooks, Cloud Tasks and Cloud Scheduler only prompt reconciliation. Any of them can be lost, duplicated or reordered without losing a queued job or leaking a VM.

## Endpoints and callers

| Route | Caller | Authentication |
| --- | --- | --- |
| `POST /webhook` | GitHub "Workflow jobs" webhook | HMAC signature of the source named in `?src=` |
| `POST /create_vm` | Create queue (Cloud Tasks) | Google OIDC token for the `runner-callback` service account |
| `POST /delete_vm` | Delete queue (Cloud Tasks) | Google OIDC, as above |
| `POST /recreate_vm` | Shutdown script on a runner VM | HMAC over an expiring, generation-bound capability |
| `POST /reconcile` | Scheduler every 2 minutes; maintenance queue for later pages | Google OIDC |
| `POST /sweep` | Scheduler every 2 minutes | Google OIDC |
| `POST /discover` | Scheduler every 5 minutes; maintenance queue for later pages | Google OIDC |
| `POST /audit` | Scheduler hourly, at minute 17 | Google OIDC |
| `GET /healthcheck` | Cloud Run | None |

OIDC callers must present a verified token for the callback service account with the Cloud Run origin as audience. Every route caps request bodies at 1 MiB. The server sets read, write and idle timeouts, and logs event identifiers rather than webhook bodies.

Three Cloud Tasks queues keep slow work from starving cleanup:

- **Create queue**: concurrency and rate come from `create_concurrency` (default 4) and `create_dispatches_per_second` (default 2).
- **Delete queue**: 4 concurrent, 4 per second, so deletions never wait behind capacity-bound inserts.
- **Maintenance queue**: 2 concurrent, 2 per second, for reconciliation and discovery pages.

The create and delete queues share one bounded retry policy: 4 attempts within 120 seconds, 10–30 second backoff. Terraform passes the same values to the autoscaler as `TASK_RETRY_*`.

## Job flow

1. **Queued webhook.** The autoscaler verifies the signature and matches the job against `github_runner_label_groups`. It rejects the legacy `@machine:` label with HTTP 422. It writes `jobs/<key>`, then enqueues a create task after `machine_creation_delay`. The Firestore write is the acknowledgement; a failed enqueue is picked up by the next reconciliation pass.
2. **Create worker.** The worker takes a transactional lease on the job and reads the job's current status from GitHub. For a job still queued, it first tries to adopt an idle detached runner from the same pool. Otherwise it reserves a fleet slot, generates a JIT runner configuration, and inserts a VM named `<github_runner_prefix>-<job id>-<16 hex characters>`. Zone, template, machine type, JIT configuration and Compute request ID are persisted before the insert, so a retried callback resumes the same attempt.
3. **Capacity fallback.** The worker tries every zone, skipping zones benched by the zone circuit breaker until last. When a job has no `gce-machine-*` label it walks `machine_type_fallbacks`. When the primary template is SPOT and SPOT is exhausted everywhere, it falls back to the STANDARD template, subject to `allow_on_demand` and `max_on_demand_runners`.
4. **In-progress webhook.** The autoscaler records which runner took the job (see [Assignment evidence](#assignment-evidence)).
5. **Completed webhook.** The job becomes a terminal record with a pending deletion for the reported runner. The delete worker deletes the VM and releases its reservation.
6. **Lost capacity.** A VM's shutdown script runs on SPOT preemption, self-shutdown and deletion. If the runner never accepted a job, the script posts to `/recreate_vm`. The autoscaler accepts the request only if the capability is unexpired and the job record is non-terminal and still owns that exact VM. It then schedules a create pass in 45 seconds, which replaces capacity only if GitHub still reports the job as queued.

## Demand, capacity and reconciliation contract

`jobs/<key>` contains durable demand and an optional claim on a runner generation. The key is the repository and job ID, so a job delivered by overlapping webhook scopes (an organization webhook plus a repository webhook) converges on one record owned by the first registered source that delivered it. `runners/<generation>` is the capacity ledger, including generations detached from their originating job. A claim is not a GitHub assignment. Workers consult durable assignment evidence before using GitHub runner REST status/busy as a secondary hint. The GitHub runner ID from JIT registration is retained, making this a direct GET rather than repeatedly scanning all registrations.

If A's runner RA is busy with B, A relinquishes its claim while RA stays counted in the ledger. B relinquishes its unused RB when GitHub reports B in progress. An idle detached RB can then be claimed transactionally by A without adding capacity. If RB was preempted, A creates replacement capacity on a subsequent due pass while RA continues serving B. Available capacity is shared conservatively within the same source, repository and case-normalized label set; no repository-eligibility assumption is made across pools. Completion deletes the actual reported generation. Detached generations are released idempotently after confirmed deletion/absence, never merely because a job relinquishes a claim.

Fleet admission is transactional. `control/fleet` counts every reservation, including pending and ambiguous inserts and stopped VMs awaiting cleanup, against `max_runners`; STANDARD reservations also count against `max_on_demand_runners`. Only reservation and provisioning-model transitions read and update `control/fleet`; ordinary webhook, lease, scheduling and status writes do not touch the global counter. A zonal insert with an uncertain outcome is resolved through its Compute operation before its reservation is released.

The hot query uses the explicit indexed pair `NeedsReconcile=true` and `NextActionAt<=now`, ordered by due time and document ID. Settled terminal records keep a seven-day TTL but never enter that query. Terminal records with pending deletion or an unreleased claim remain actionable. Queued demand normally checks after two minutes; in-progress demand and healthy detached runners check after ten minutes, with completion webhooks and stopped-VM sweeps accelerating cleanup.

`EnqueueToken` and `EnqueuedUntil` cover one bounded retry chain, sized from `TASK_RETRY_*`. The marker is written before enqueue; a crash or ambiguous enqueue result is recovered after marker expiry. Workers reject obsolete dispatch tokens. Lease contention is acknowledged, fleet saturation schedules a later attempt, and permanent configuration failures (for example a `gce-machine-*` type missing from `allowed_machine_types`) are persisted and alerted instead of hot-retried. After correcting a parked configuration failure, an operator must clear that job's `Failure` field and set its next action due. Active reservations still reconcile for cleanup.

A JIT configuration is discarded if its VM has not been inserted within 45 minutes, so it cannot outlive a prolonged capacity outage.

## Discovery

Discovery reconstructs queued demand whose webhook never arrived. Every five minutes it pages through the organization's visible repositories, or through `discovery_repositories` when set. For each repository it lists workflow runs created in the last three days once and filters to unfinished runs client-side. Each page is a bounded, retryable task on the maintenance queue.

Enterprise installations must supply `discovery_repositories`. Organization installations can also set it to bound API usage. A GitHub 404 fails closed, because it can indicate missing access.

## GitHub rate limits and permissions

GitHub primary and secondary rate-limit responses honor `Retry-After` and the reset time through a persisted, PAT-wide not-before gate. Permission errors, including masked job-status 404s, retain demand and pause that endpoint path for fifteen minutes. Every page of a listing shares the pause, while sibling endpoints under the same repository (job status, runner registration) stay independent. Discovery acknowledges deferred pages, and the next scheduled traversal reconstructs them after the gate opens. Network and 5xx failures use the bounded Cloud Tasks retry policy. The gates survive process termination and scale-to-zero.

## Runner registration states

A runner-by-ID 404 triggers a runner-inventory check before classification. A successful inventory that lacks the generation proves registration absence without setting repository backoff; a failed inventory retains the VM and follows normal permission/rate/transient handling. Queued demand loses a disappeared registration's VM and receives replacement capacity on its next due dispatch. Completed demand deletes an idle or unregistered VM; busy runners remain detached and counted. Detached generations are also checked for registration disappearance, so a lost completion webhook does not leave them running until `machine_timeout`. Job-status 404s continue to fail closed.

Both runner lookup paths require GitHub `status=online` before treating a registration as idle or busy. Offline (including unknown status) counts as provisioning only during `runner_register_timeout`, measured from the persisted VM creation time, with attempt/JIT time as recovery fallbacks. Once that grace expires, reconciliation deletes the VM and releases its reservation so queued demand can receive a replacement. Completed offline runners are deleted immediately. Offline detached runners are never advertised as available; their availability is refreshed transactionally during reconciliation, and adoption always rechecks current registration state before retaining capacity.

## Assignment evidence

Owned `runner_name` observations from in-progress webhooks and discovery are persisted at `runners/<generation>/assignments/job`. This subdocument survives owner snapshot writes, transfers and parent deletion. The durable job write precedes the assignment write; webhook retries and discovery repair an interruption between them. Assignment transactions consult the durable terminal job state, so delayed in-progress events cannot resurrect completion. Completed assignment records expire after seven days.

Positive assignment evidence overrides the runner REST busy, online, offline and absence hints. Reconciliation checks the assigned job's status; while completion is unconfirmed, the generation remains counted, unavailable to originating demand, and protected from lifecycle deletion. Permission and network errors retain that protection. After confirmed completion, the JIT generation is consumed and reclaimed even if REST still reports it idle. Detached reconciliation and deletion callbacks use the same protection. Assignment writes, detach, adoption and availability refresh transact against the assignment observation, so capacity snapshots cannot advertise a known assigned runner as a spare.

## Orphan sweep

Every two minutes the sweep lists VMs whose names start with `github_runner_prefix` in every configured zone. It deletes stopped VMs and reconciles detached runner generations. Sweeping runs on the schedule, independent of webhook traffic.

## Invariant audit and repair

The hourly audit reads a consistent runner-ledger/counter snapshot, inventories matching GCE generations, and checks the counter revision again. If capacity changed during the external inventory, it defers rather than report a transient inconsistency. It alerts on high/low counter drift, unexpected GCE generations, provisioning-model mismatches and confirmed missing VMs. Pending and ambiguous inserts legitimately count even when no VM is visible. The audit scans live reservations, not job records.

Repair is operator-controlled. The autoscaler has no automatic repair endpoint, because one could breach the fleet ceiling under concurrent inserts. To repair:

1. Pause all lifecycle queues and schedulers, including deletion and sweeping, and quiesce webhook writers.
2. Wait for active requests, leases and Compute operations to settle.
3. Resolve unmanaged generations and ambiguous inserts.
4. Reconcile the ledger against actual generations, then rebuild both counters from the settled ledger.
5. Resume queues and schedulers.

Never overwrite the counter with the instantaneous GCE VM count, and never repair while lifecycle writers are active. Never reset counters independently of reservations.

## State schema

The autoscaler checks schema version 1 at startup (`control/schema`) and on each job or runner read used for a mutation. Unknown versions fail closed and emit initialization or lifecycle alerts. Without a schema marker, any existing jobs, runners or `control/fleet` document causes rejection rather than silent adoption. Future semantic changes must increment the version and ship a migration. Rolling back across versions requires restoring or migrating to a compatible database. Tracking `master` does not waive this contract.

Completed job records expire after seven days, and only once their reservations and pending deletion have cleared. Active demand has no TTL. Terraform abandons the named database on destroy instead of deleting it; reimport it before recreating infrastructure.

## Operations

- **Scale to zero.** Cloud Run runs with `min_instance_count=0` and `max_instance_count=3`. Scheduler jobs wake it every two minutes (reconcile, sweep), every five minutes (discover) and hourly (audit). No correctness requirement depends on a warm process. Scheduled requests, Firestore operations and active processing still incur usage; production cost has not been measured.
- **Monitoring.** Alert policies cover VM creation failures, JIT registration failures, Compute write-rate throttling, benched zones, and lifecycle attention: delete, sweep, discovery, audit or reconciliation errors, initialization failures, and queued jobs pending for more than 15 minutes. Attach notification channels with `alert_notification_channels`.
- **Before changing limits**, check queue age, lifecycle errors, stopped VMs and reservations. Fleet saturation is acknowledged and deferred, so a full fleet shows up as pending jobs, not errors.
- **Releases.** Consumers that track `master` pick up module and image changes on their next `terraform apply`. The module and image are resolved separately, so publish the image before deploying a consumer; the two lookups are not an atomic release. Immutable `sha-<commit>` and date-version tags remain available for rollback.

## Upgrading from a pre-durable release

Releases before the durable lifecycle kept no Firestore state and authenticated worker callbacks with HMAC.

- Drain existing runners before upgrading, or budget for them explicitly. Admission counters cover durable reservations only, not VMs created by the older release.
- The new release rejects the old HMAC worker callbacks. Scheduled discovery reconstructs queued demand.
- Running old VMs keep their `max_run_duration` and are swept once stopped.
- If a build that predates schema version 1 ever ran against the database, drain it and migrate state explicitly before deploying.

## Validation

From `runner-autoscaler`, run the unit and HTTP-level tests:

```bash
go test -race ./pkg/... ./test/... -skip '^(TestCreateCallbackTask|TestGenerateRunnerJitConfig|TestDeleteNotExistingVM)$'
```

The skipped cases require live credentials and can create tasks or runner registrations. Regression cases cover concurrent workers, cancellation and reordering, fleet admission, API outages, expired leases, ambiguous inserts, JIT refresh, pending deletion, request bounds and callback scope.

CI additionally runs the `TestFirestore*` tests against a Firestore emulator, covering capacity transfer, version guards, due pagination and job-only writes. It also runs `terraform validate` against a consuming-root fixture with a local module source, and a plan-time ghcr.io digest-resolution smoke test. None of these plan or apply live infrastructure. The emulator cannot establish production IAM correctness or throughput, so validate IAM/OIDC and Scheduler delivery in staging and observe a full create/run/delete cycle before deploying.

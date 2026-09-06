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

- The consuming repo must pin both this module's commit and its matching `sha-<commit>` container tag. Branch CI publishes only that immutable commit tag; master alone advances release tags.
- Drain existing runner generations before migration, or explicitly budget for them. The new admission counters cover durable reservations, not VMs created by older releases. Existing HMAC worker callbacks are rejected after migration; scheduled discovery reconstructs queued demand. Running old VMs retain their runtime limit and are swept when stopped.
- Completed records expire after seven days only after reservations and pending deletion have cleared. Active demand has no TTL. The named database is abandoned rather than deleted on Terraform destroy. Reimport it before recreating infrastructure; never reset counters independently of reservations.
- Fleet saturation and transient API failures return retryable failures; durable reconciliation outlives the Cloud Tasks retry window. Operators should check queue age, lifecycle errors, stopped VMs and Firestore reservations before changing limits.
- The module creates three Scheduler jobs and keeps one small Cloud Run instance warm. This adds a fixed operating cost in exchange for predictable webhook intake. Fleet limits, restricted overrides, conservative on-demand fallback and independently throttled creation bound variable exposure. No production performance or savings claims have been measured.
- Monitoring includes creation/JIT/rate/zone failures, deletion/sweep/discovery errors, and queued records older than 15 minutes. Supply notification channels in the consuming repo.

## Validation

Run `go test -race ./pkg/... ./test/... -skip '^(TestCreateCallbackTask|TestGenerateRunnerJitConfig|TestDeleteNotExistingVM)$'` from `runner-autoscaler`. The skipped cases require live credentials and can create tasks or registrations. Regression cases cover concurrent workers, cancellation/reordering, fleet admission, API outages, expired leases, ambiguous inserts, JIT refresh, pending deletion, request bounds and callback scope.

Terraform validation uses a consuming-root fixture with a local module source and dummy Google provider settings; it does not plan or apply live infrastructure. Before deployment, validate real IAM/OIDC, Firestore transaction behavior and Scheduler delivery in staging, then observe a full create/run/delete cycle. Unit tests use an in-memory transactional store and cannot establish production IAM correctness or throughput.

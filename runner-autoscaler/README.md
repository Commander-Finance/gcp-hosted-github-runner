# Autoscaler

#### Creates and deletes runner VMs from GitHub workflow job webhooks

The autoscaler is a Cloud Run service. It records each GitHub "Workflow jobs" webhook in Firestore and answers within GitHub's [10 second webhook timeout](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks#respond-within-10-seconds). Slow work (creating and deleting VMs) runs in Cloud Tasks callbacks with a longer deadline (`TASK_DISPATCH_TIMEOUT`). Cloud Scheduler runs reconciliation, orphan sweeps, discovery and an invariant audit. [LIFECYCLE.md](../LIFECYCLE.md) documents every route, the Firestore data model and the operational contract.

`main.go` requires `STATE_DATABASE`, so production always serves the durable, Firestore-backed handlers. The older stateless handlers in `pkg/srv.go` are reachable only from tests.

### Scaling rules

> [!IMPORTANT]
> If the scaler is configured incorrectly, this can lead to “dangling” computing instances, resulting in unnecessary costs.

The autoscaler records a workflow job only when all of these hold:

* The request names a configured source (`GITHUB_ENTERPRISE`, `GITHUB_ORG`, `GITHUB_REPOS`) in the `SOURCE_QUERY_PARAM_NAME` query parameter, and the webhook signature is valid.
* The event is `workflow_job`.
* The job's labels fully satisfy at least one label group in `RUNNER_LABELS` (OR-of-ANDs, case-insensitive).
* The job does not use the legacy `@machine:` label. Such jobs are rejected with HTTP 422.

A `queued` job receives a VM only if GitHub still reports it as queued when the create worker runs, and a slot is free under `MAX_RUNNERS`. A `completed` job deletes the runner it reports, if that runner's name carries `RUNNER_PREFIX`.

### Configuration

Terraform sets every variable below from the module inputs (`cloudRun.tf`). Variables marked **required** have no default; the process panics at startup without them.

#### State, callbacks and queues

| Env | Default | Description |
| --- | --- | --- |
| STATE_DATABASE | **required** | Firestore database ID holding lifecycle state (`github-runners`). |
| CALLBACK_BASE_URL | **required** | HTTPS origin of this Cloud Run service. Used as the callback URL base and the expected OIDC audience. |
| CALLBACK_SERVICE_ACCOUNT | **required** | Service account email that Cloud Tasks and Cloud Scheduler use to sign OIDC tokens. Callers presenting any other identity are rejected. |
| TASK_QUEUE | **required** | Relative resource name of the create queue. |
| DELETE_TASK_QUEUE | **required** | Relative resource name of the delete queue. |
| MAINTENANCE_TASK_QUEUE | **required** | Relative resource name of the queue for reconciliation and discovery pages. |
| TASK_DISPATCH_TIMEOUT | "180" | Callback deadline in seconds. Must be between 30 and 1700. |
| TASK_RETRY_ATTEMPTS | "4" | Mirrors the create/delete queue retry policy so the dispatch marker spans one full retry chain. |
| TASK_RETRY_MAX_BACKOFF | "30" | As above, in seconds. |
| TASK_RETRY_MAX_DURATION | "120" | As above, in seconds. |
| CREATE_VM_DELAY | "10" | Seconds to wait after a `queued` webhook before the create callback runs. Lets a quickly canceled job skip VM creation. |
| MAX_REQUEST_BYTES | "1048576" | Request body limit for every route. |

#### Fleet limits

| Env | Default | Description |
| --- | --- | --- |
| MAX_RUNNERS | "100" | Admission limit for runner generations, including pending inserts and stopped VMs awaiting cleanup. |
| MAX_ON_DEMAND_RUNNERS | "10" | Admission limit for STANDARD (non-SPOT) VMs. Must not exceed `MAX_RUNNERS`. |
| ALLOW_ON_DEMAND | "1" | "0" forbids STANDARD VMs. A STANDARD-only fleet (no `INSTANCE_TEMPLATE_FALLBACK`) requires "1" and a positive `MAX_ON_DEMAND_RUNNERS`. |
| ALLOWED_MACHINE_TYPES | "" *(space separated)* | Machine types a job may request with a `gce-machine-*` label. Empty rejects every override as a permanent configuration failure. |
| MACHINE_TIMEOUT | "3600" | Maximum VM lifetime in seconds; also bounds the recreate capability's validity. Must be at least 60. Terraform passes `machine_timeout` (module default 14400). |
| RUNNER_REGISTER_TIMEOUT | "120" | Seconds an offline registration counts as provisioning before reconciliation deletes the VM. |

#### Compute

| Env | Default | Description |
| --- | --- | --- |
| PROJECT_ID | **required** | Google Cloud project ID. |
| ZONES | **required** *(comma separated)* | Zones the autoscaler may create VMs in. |
| INSTANCE_TEMPLATE | **required** | Relative resource name of the primary instance template. |
| INSTANCE_TEMPLATE_FALLBACK | "" | STANDARD template tried when the primary SPOT template is out of capacity in every zone. Empty when the primary is already STANDARD. |
| RUNNER_MACHINE_TYPE_FALLBACKS | "" *(comma separated)* | Ordered machine types tried when a job has no `gce-machine-*` label and the current type is out of capacity. Every entry must support the template's disk type. |
| RUNNER_PREFIX | "runner" | VM name prefix. VMs are named `<prefix>-<job id>-<16 hex characters>`. Must be GCE-safe and at most 20 characters. |
| ZONE_BENCH_MIN_VMS | "3" | Zone circuit breaker: a zone is tried last when at least this many of its runner VMs logged an outbound dial timeout (the Ops Agent's `Exporting failed ... i/o timeout` syslog line) in the last `ZONE_HEALTH_WINDOW` seconds. Read from Cloud Logging on the create path, cached 60 s, fails open. Needs `logging.logEntries.list` and a runner image that ships the Ops Agent. "0" disables the breaker. |
| ZONE_BENCH_MIN_RATIO | "0.2" | Zone circuit breaker: the failing VMs must also be at least this fraction of the VMs created in that zone in the window. Must be in (0, 1]. |
| ZONE_HEALTH_WINDOW | "600" | Zone circuit breaker lookback in seconds. |

#### GitHub

| Env | Default | Description |
| --- | --- | --- |
| SECRET_VERSION | **required** | Relative resource name of the Secret Manager version holding the PAT. |
| GITHUB_ENTERPRISE | "" | Enterprise name and base64-encoded webhook secret, separated by `;`. |
| GITHUB_ORG | "" | Organization name and base64-encoded webhook secret, separated by `;`. |
| GITHUB_REPOS | "" *(comma separated)* | `OWNER/REPO;<base64 secret>` pairs, separated by `,`. |
| SOURCE_QUERY_PARAM_NAME | "src" | Query parameter that names the webhook source on every request. |
| RUNNER_GROUP_ID | "1" | Runner group that enterprise and organization runners join. |
| RUNNER_LABELS | "self-hosted" | Label groups, OR-of-ANDs. Groups are separated by `;`, labels within a group by `,`. Whitespace is trimmed per label; magic labels (`gce-machine-*`) are skipped per group. Examples: `"builder"`, `"builder,linux"`, `"builder;builder-large"`, `"builder,linux;builder-large,linux"`. Zero parsed groups rejects every webhook and logs a warning. |
| DISCOVERY_REPOSITORIES | "" *(space separated)* | `OWNER/REPO` names that discovery scans. Empty scans every organization repository visible to the PAT. Required for enterprise sources. |
| RUNNER_JOB_LOG_PATTERN | "Running job:" | journald substring that marks "this runner accepted a job". Baked into each VM's metadata for its shutdown script. Must match the Terraform variable `runner_job_log_pattern`. |

#### Routes and process

| Env | Default | Description |
| --- | --- | --- |
| ROUTE_WEBHOOK | "/webhook" | Path GitHub calls. |
| ROUTE_CREATE_VM | "/create_vm" | Create callback path. |
| ROUTE_DELETE_VM | "/delete_vm" | Delete callback path. |
| ROUTE_RECREATE_VM | "/recreate_vm" | Path a runner VM's shutdown script calls when it dies without accepting a job. |
| PORT | "8080" | Listen port. |
| DEBUG | "0" | "1" enables debug logs. Secrets may be leaked. |
| SIMULATE | "0" | "1" records jobs but creates and deletes no VMs. Development only. |

The `/reconcile`, `/sweep`, `/discover` and `/audit` paths are fixed.

### Tests

From this directory:

```bash
go test -race ./pkg/... ./test/... -skip '^(TestCreateCallbackTask|TestGenerateRunnerJitConfig|TestDeleteNotExistingVM)$'
```

The skipped tests need live GCP and GitHub credentials. The `TestFirestore*` tests need a Firestore emulator; set `FIRESTORE_EMULATOR_HOST` to run them.

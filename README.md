# gcp-hosted-github-runner

[![GitHub Actions Workflow Status](https://img.shields.io/github/actions/workflow/status/Commander-Finance/gcp-hosted-github-runner/main.yml?branch=master&style=flat&logo=github&label=Docker+build)](https://github.com/Commander-Finance/gcp-hosted-github-runner/actions?query=branch%3Amaster)
[![awesome-runners](https://img.shields.io/badge/listed%20on-awesome--runners-blue.svg)](https://github.com/jonico/awesome-runners)


☁️☁️☁️ **This terraform module provides a ready to use solution for Google Cloud hosted [GitHub ephemeral runner](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/autoscaling-with-self-hosted-runners#using-ephemeral-runners-for-autoscaling)** ☁️☁️☁️

> [!IMPORTANT]
> I am not responsible if this Terraform module results in high costs on your billing account. Keep an eye on your billing account and activate alerts!

## Quickstart

#### 1. Apply Terraform
Add this Terraform module to your root module and provide/adjust the values:

``` hcl
provider "google" {
  project = "<gcp_project>"
  region  = "<gcp_region>"
  zone    = "<gcp_zone>"
}

module "github-runner" {
  source                    = "github.com/Commander-Finance/gcp-hosted-github-runner"
  machine_type              = "c2d-highcpu-8" // The default machine type of the VM instance.
  github_runner_group_id    = 1 // The GitHub Organization/Enterprise runner group ID. Has no effect for GitHub Repositories.

  // Provide only ONE of the following variables:
  github_enterprise         = "<enterprise_name>" // Provide the name of the GitHub Enterprise.
  github_organization       = "<organization_name>" // Provide the name of the GitHub Organization.
  github_repositories       = ["<repository_user/repository_name>"] // Provide USER/NAME of at least one GitHub Repository.
}

output "runner_webhook_config" {
  value = nonsensitive(module.github-runner.runner_webhook_config) // Remove the output after the initial setup.
}
```

The module requires Terraform 1.9 or newer. Authenticate with `gcloud` and apply the terraform module. On a brand-new GCP project, the first `apply` may fail with an API-not-enabled or NotFound error while newly enabled Google APIs (Cloud Run, Artifact Registry, Cloud Tasks, Secret Manager, Compute, Firestore, Cloud Scheduler, Monitoring) finish propagating — wait a minute and re-run `apply`.

The module creates a Firestore database named `github-runners` in the runtime region (override with `firestore_location`). Terraform abandons the database on destroy rather than deleting it, and its location cannot be changed in place.

``` bash
$ gcloud auth application-default login --project <gcp_project>
$ terraform init -upgrade && terraform apply
```

> [!IMPORTANT]
> After a successful initial setup you should remove the `runner_webhook_config` output because it prints the webhook secret(s). Also make sure that the Terraform state file is stored in a safe place (e.g. in a private [Cloud Storage bucket](https://cloud.google.com/docs/terraform/resource-management/store-state)). The state file contains the webhook secret as plaintext.

#### Pinning the runner image

Every push to `master` builds a new autoscaler image, publishes it to `ghcr.io/commander-finance/github-runner-autoscaler`, and cuts a matching GitHub release tagged with a UTC timestamp version `YY.MM.DD.HHMMSS` (e.g. `26.04.14.152345`). The Terraform module resolves the selected image tag to an immutable `sha256` digest by querying **ghcr.io directly at plan time**, and pins Cloud Run to `<artifact-registry-path>@sha256:<digest>`. At runtime, Cloud Run pulls that digest **through the Artifact Registry remote-repo proxy** (`dockerRepository.tf`), which lazily caches the layers from ghcr.io on first pull. So `terraform apply` rolls a new Cloud Run revision only when ghcr.io's manifest for the selected tag has actually changed, and AR provides in-region caching for subsequent pulls without gating plan-time resolution.

Three pinning modes:

**1. Track master (default).** Omit `runner_image_tag`; it defaults to `"master"`. Each `terraform plan` resolves `:master` to the current digest — new merges to master automatically redeploy Cloud Run on the next apply.

``` hcl
module "github-runner" {
  source = "github.com/Commander-Finance/gcp-hosted-github-runner"
  # runner_image_tag defaults to "master"
  # ...
}
```

**2. Pin a release.** Pick a release from the [releases page](https://github.com/Commander-Finance/gcp-hosted-github-runner/releases) and set both the module ref and the image tag to match:

``` hcl
module "github-runner" {
  source           = "github.com/Commander-Finance/gcp-hosted-github-runner?ref=26.04.14.152345"
  runner_image_tag = "26.04.14.152345"
  # ...
}
```

**3. Pin a specific commit (debug).** Use the immutable per-commit tag:

``` hcl
runner_image_tag = "sha-<full-commit-sha>"
```

If you need to force a Cloud Run revision without changing the image (e.g., after rotating the PAT secret), set `force_cloud_run_deployment = true` on the next apply and unset it afterward.

#### 2. Configure GitHub webhook

Have a look at the Terraform output `runner_webhook_config`. There you find the Cloud Run webhook payload url(s) and the associated webhook secret(s). For each output line you have to create either an [Enterprise](https://docs.github.com/en/enterprise-cloud@latest/webhooks/using-webhooks/creating-webhooks#creating-a-global-webhook-for-a-github-enterprise), [Organization](https://docs.github.com/en/webhooks/using-webhooks/creating-webhooks#creating-an-organization-webhook) or [Repository](https://docs.github.com/en/enterprise-cloud@latest/webhooks/using-webhooks/creating-webhooks#creating-a-repository-webhook) webhook:
* Fill in the Payload URL (from the Terraform output)
* Select Content type "application/json"
* Fill in the Secret (from the Terraform output)
* Enable SSL verification
* Select "Let me select individual events":
  * Make sure everything is deselected and then select "Workflow jobs" (at the bottom)
* Check "Active"
* Click "Add webhook"

#### 3. Provide PAT

* For an **Enterprise**: Create a [Personal access token (PAT classic)](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#creating-a-personal-access-token-classic) with the "manage_runners:enterprise" scope.
* For an **Organization**: Create a [Fine-grained personal access token (PAT)](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#creating-a-fine-grained-personal-access-token) with the **Organization** Read/Write permission "Self-hosted runners". 
* For **Repositories**: Create a [Fine-grained personal access token (PAT)](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens#creating-a-fine-grained-personal-access-token) with the **Repository** permissions Read/Write "Administration".

Discovery and job-status checks also need the PAT to have **Actions: Read** on every participating repository.

This PAT is needed to automatically create a [Enterprise](https://docs.github.com/en/enterprise-cloud@latest/rest/actions/self-hosted-runners?apiVersion=2022-11-28#create-configuration-for-a-just-in-time-runner-for-an-enterprise), [Organization](https://docs.github.com/en/rest/actions/self-hosted-runners?apiVersion=2022-11-28#create-configuration-for-a-just-in-time-runner-for-an-organization), [Repository](https://docs.github.com/en/rest/actions/self-hosted-runners?apiVersion=2022-11-28#create-configuration-for-a-just-in-time-runner-for-a-repository) jit-config for each ephemeral runner to join the Repository or the runner group of an Enterprise/Organization. Then open the [Secret Manager](https://console.cloud.google.com/security/secret-manager) in the Google Cloud Console and add a new Version to the already existing secret "github-pat-token". Paste the PAT into the Secret value field and click "ADD NEW VERSION".

> [!TIP]
> Currently it is only possible to provide **one** PAT to the secret. That's why you can't combine an Enterprise with an Organization or Repository.

That's it 👍

As soon as you start a GitHub workflow whose job's `runs-on` labels fully satisfy all non-magic labels of at least one label group configured via [`github_runner_label_groups`](./variables.tf) (default `[["self-hosted"]]`), a VM instance with the specified `machine_type` starts. The VM is named `<github_runner_prefix>-<job id>-<random suffix>`; the same name is the runner's name in the GitHub runner group or repository. The spawned runner registers with the **job's** full `runs-on` labels — the groups are a webhook filter, not the registered label set. After the workflow job completes, the VM instance is deleted.

> [!NOTE]
> Two label-disjoint groups (e.g. `[["builder"], ["builder-large"]]`) let one autoscaler serve two pools without GitHub's scheduler cross-assigning runners. See [Multiple label-disjoint pools](#multiple-label-disjoint-pools) below.

## Advanced Configuration

Have a look at the [variables.tf](./variables.tf) file how to further configure the Terraform module.

These are the most common variables you may want to change:

`max_runners`: The fleet ceiling (default 100), counted from durable reservations, including pending inserts and stopped VMs awaiting cleanup. When the fleet is full, queued jobs wait for capacity. `max_concurrency` is deprecated and ignored.

`max_on_demand_runners` / `allow_on_demand`: The ceiling on STANDARD (non-SPOT) VMs (default 10) and whether they are allowed at all (default `true`). With `machine_preemtible = true`, STANDARD VMs are only a fallback when SPOT capacity is exhausted. A non-preemptible fleet requires `allow_on_demand = true` and a positive `max_on_demand_runners`.

`github_runner_label_groups`: One or more label groups the autoscaler matches against incoming workflow jobs (OR-of-ANDs — a job matches if it carries ALL non-magic labels of ANY one group; `gce-machine-*` labels are ignored for group matching). Examples: `[["self-hosted"]]` (default single-pool), `[["self-hosted", "linux"]]` (single pool, two required labels), `[["builder"], ["builder-large"]]` (two disjoint pools served by one autoscaler).

`machine_type`: The VM instance machine type where the GitHub runner will run on by default (can be individually overwritten per workflow job, see [Magic Labels](#magic-labels)).

`allowed_machine_types`: The machine types a job may request with a `gce-machine-*` label. Empty (the default) disables per-job overrides.

`machine_type_fallbacks`: An ordered list of machine types to try when a job has no `gce-machine-*` label and the current type is out of capacity. Every entry must support the configured `disk_type`.

`discovery_repositories`: `owner/repo` names that scheduled discovery scans for queued jobs whose webhook was lost. Empty scans every organization repository visible to the PAT. Required for Enterprise installations.

`create_concurrency` / `create_dispatches_per_second`: Concurrency (default 4) and rate (default 2/s) of VM-create callbacks. Tune against your Compute write quotas.

`alert_notification_channels`: Cloud Monitoring notification channel IDs for the autoscaler's alert policies.

`disk_size_gb`: The size of the VM disk.


> [!TIP]
> To find the cheapest VM machine_type use this [table](https://gcloud-compute.com/instances.html) and sort by Spot instance cost. But remember that the price varies depending on the region.

## Runner features

* Executed by unprivileged user with name `agent` with the default uid `10000` and gid `10000`. Can be changed with `github_runner_uid`.
* Provides docker-daemon and docker-buildx by default. Additional packages can be installed with `github_runner_packages`.
* Only works with images that are based on debian (rely on apt package manager). Runs image `ubuntu-minimal-2204-lts` by default. Change with `machine_image`.

### Multiple label-disjoint pools

A single autoscaler can serve multiple workflow-job populations by configuring `github_runner_label_groups` with more than one group. The autoscaler accepts a job if it matches **any** group; the spawned runner registers with the **job's** `runs-on` labels (not the group's). For pool isolation, make the groups disjoint — otherwise GitHub's scheduler may route a job into the wrong pool.

```hcl
github_runner_label_groups = [
  ["builder"],         # default-sized VMs for runs-on: builder
  ["builder-large"],   # custom-sized VMs for runs-on: [builder-large, gce-machine-<type>]
]
```

Per-pool defaults (disk size, image, preemptibility, runner group, fleet limits) are **not** supported — the instance template is shared across all groups. Only `machine_type` diverges, via the per-job `gce-machine-*` magic label below.

### Magic Labels

Each workflow job can select a different machine type than the configured default `machine_type`. Use the special label `gce-machine-<type>`, e.g. `gce-machine-c2d-standard-16`. The type must be listed in `allowed_machine_types`; the autoscaler parks a job that requests any other type as a configuration failure and creates no VM. Make sure the configured `disk_type` is supported by the machine.

```yaml
jobs:
  example:
    runs-on: [self-hosted, gce-machine-c2d-standard-16]  # runs on a c2d-standard-16 VM
    steps:
      - run: echo Hello world!
```

> [!NOTE]
> Earlier versions of this module documented `@machine:<type>` (e.g. `@machine:c2d-standard-16`). That syntax does not work: GitHub's JIT runner-registration API rejects labels containing `@` or `:`, so runners spawned for such jobs could not match the job's required labels and the job timed out. Replace `@machine:<type>` with `gce-machine-<type>` in any existing workflows. The webhook handler rejects jobs that still use the old syntax with HTTP 422 (visible under the webhook's Recent Deliveries) and creates no VM.

## Expected Cost

The following Google Cloud resources are created that may generate cost:
* Cloud Task (covered by Free Tier)
* Secret Version (covered by Free Tier)
* Artifact Registry (covered by Free Tier)
* Cloud Run (scales to zero, but scheduled maintenance wakes it every two minutes)
* Firestore (per-operation charges for lifecycle state)
* Cloud Scheduler (four jobs)
* Cloud Monitoring log-based metrics and alert policies
* (Spot) VM Instance(s) + standard persistent disk + ephemeral external IPv4

Other:
* Egress network traffic (200 GiB/month is free)

**Example:**

A single 1 h long workflow job in europe-west1 leads to the following cost:

```
Ephemeral external IPv4 for Spot instance $0.0025
Spot VM Instance c2d-highcpu-8            $0.0494
Standard persistent disk 20 GiB used    ~ $0.0011
-------------------------------------------------
                                          $0.053
```

Overall, the compute instance accounts for the majority of the costs. The baseline cost of scheduled maintenance, Firestore and Cloud Run has not been measured in production.

## How it works

The autoscaler keeps every queued job and every runner VM in Firestore, so a lost, duplicated or reordered webhook cannot lose a job or leak a VM. [LIFECYCLE.md](LIFECYCLE.md) documents the full lifecycle contract, operations and repair procedure.

1. A GitHub "Workflow jobs" webhook calls the Cloud Run path `/webhook`. The autoscaler verifies the signature, matches the job's labels, records the job in Firestore, and enqueues a create task after `machine_creation_delay` (default 10 seconds).
2. The create task calls `/create_vm` with a Google OIDC token. The autoscaler takes a lease on the job and asks GitHub whether the job is still queued. If it is, the autoscaler reuses an idle spare runner from the same pool or reserves a slot under `max_runners`.
3. The autoscaler creates a JIT runner configuration with the PAT from Secret Manager, then creates the VM from the instance template with the JIT configuration in its metadata. It tries every zone, then `machine_type_fallbacks`, then an on-demand (STANDARD) template when SPOT capacity is exhausted everywhere.
4. The runner registers, waits for GitHub to dispatch a job, and runs it. A runner that never comes online shuts down after `runner_register_timeout`; an online runner that is never dispatched shuts down after `runner_job_dispatch_timeout`.
5. When the job completes, the webhook marks the job terminal and enqueues a delete task. `/delete_vm` deletes the VM and releases its reservation.
6. If a VM dies before accepting a job (for example, SPOT preemption), its shutdown script calls `/recreate_vm`. The autoscaler replaces the capacity if GitHub still reports the job as queued.

Cloud Scheduler drives recovery independently of webhook traffic:

* `/reconcile` (every 2 minutes) re-dispatches every job record that is due for action.
* `/sweep` (every 2 minutes) deletes stopped runner VMs and reconciles spare runners.
* `/discover` (every 5 minutes) lists recent workflow runs and records queued jobs whose webhook never arrived.
* `/audit` (hourly) compares the fleet counters with the actual VMs and alerts on drift.

Every VM also carries a `max_run_duration` of `machine_timeout`, so Compute deletes it even if every other mechanism fails.

## Troubleshooting

> [!TIP]
> If something does not work as expected have a look in the Logs of the github-runner-autoscaler Cloud Run.

#### Public access to Cloud Run disallowed

The terraform error looks something like this:
```
Error applying IAM policy for cloudrun service "v1/projects/my-gcp-project-id/locations/us-east1/services/cloudrun-service": Error setting IAM policy for cloudrun service "v1/projects/my-gcp-project-id/locations/us-east1/services/cloudrun-service": googleapi: Error 400: One or more users named in the policy do not belong to a permitted customer, perhaps due to an Organization policy
```

1. Solution: Use project tags: [How to create public Cloud Run services when Domain Restricted Sharing is enforced](https://cloud.google.com/blog/topics/developers-practitioners/how-create-public-cloud-run-services-when-domain-restricted-sharing-enforced?hl=en)

2. Solution: Override the Organization Policy "Domain Restricted Sharing" in the project, by setting it to "Allow all".

#### The VM instance stops shortly after it was created without processing a workflow task

The VM will stop itself if it does not come online within `runner_register_timeout` (default 120s), which usually means registration at the GitHub runner group failed. This can be caused by:
* A typo in the GitHub Enterprise, Organization, Repository name. Check the Terraform variables `github_enterprise`, `github_organization`, `github_repositories` for typos.
* A not existing GitHub runner group within the Enterprise/Organization. Check the Terraform variable `github_runner_group_id`.
* The GitHub runner version is [deprecated](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/autoscaling-with-self-hosted-runners#controlling-runner-software-updates-on-self-hosted-runners). The GitHub runner won't accept any Workflow job. Check the Terraform variable `github_runner_download_url` and update to latest GitHub runner version or leave empty to always use the latest version.

You can observer the runner registration process by connecting to the VM instance via SSH (see `enable_ssh`) and running:
```
$ sudo journalctl -u google-startup-scripts.service --follow
```

#### New VM Instance not created (but a lot of instances are already running)

The fleet may be at `max_runners` (or `max_on_demand_runners` for STANDARD VMs). Queued jobs then wait for capacity, and the "pending job over 15 minutes" alert fires. Raise the limit or wait for running jobs to finish.

Alternatively, you exceeded your projects vCPU limit for the machine type in the region or for all regions. You may find an error log message in the Cloud Run logs stating `Machine Type vCPU quota exceeded for region`. Request a quota increase from google customer support for the project.

#### A job that uses a `gce-machine-*` label never gets a VM

The requested type is not in `allowed_machine_types`. Add it, then resume the parked job in Firestore by clearing `Failure`, setting `NeedsReconcile` to `true`, and setting `NextActionAt` to now (see [LIFECYCLE.md](LIFECYCLE.md#demand-capacity-and-reconciliation-contract)).

#### Nothing happens at all

The job's `runs-on` labels don't fully satisfy the non-magic labels of any group in `github_runner_label_groups`. Either add the missing labels to your workflow job's `runs-on`, or add a new group to the module configuration. The autoscaler logs the parsed groups at startup and the rejection reason on every miss — check Cloud Run logs for the exact mismatch.

> [!TIP]
> When bumping the module `source` ref, also bump `runner_image_tag` so the Terraform-side encoding (`;`/`,` for label groups) and the autoscaler image's parser advance together. The Cloud Run startup log renders the parsed groups in `[a, b], [c, d]` form — eyeball that after a version bump to confirm the parser saw what Terraform sent.

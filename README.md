# gcp-hosted-github-runner

[![GitHub Actions Workflow Status](https://img.shields.io/github/actions/workflow/status/Privatehive/gcp-hosted-github-runner/main.yml?branch=master&style=flat&logo=github&label=Docker+build)](https://github.com/Privatehive/gcp-hosted-github-runner/actions?query=branch%3Amaster)
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

Authenticate with `gcloud` and apply the terraform module. On a brand-new GCP project, the first `apply` may fail with an API-not-enabled or NotFound error while newly enabled Google APIs (Cloud Run, Artifact Registry, Cloud Tasks, Secret Manager, Compute) finish propagating — wait a minute and re-run `apply`.

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

This PAT is needed to automatically create a [Enterprise](https://docs.github.com/en/enterprise-cloud@latest/rest/actions/self-hosted-runners?apiVersion=2022-11-28#create-configuration-for-a-just-in-time-runner-for-an-enterprise), [Organization](https://docs.github.com/en/rest/actions/self-hosted-runners?apiVersion=2022-11-28#create-configuration-for-a-just-in-time-runner-for-an-organization), [Repository](https://docs.github.com/en/rest/actions/self-hosted-runners?apiVersion=2022-11-28#create-configuration-for-a-just-in-time-runner-for-a-repository) jit-config for each ephemeral runner to join the Repository or the runner group of an Enterprise/Organization. Then open the [Secret Manager](https://console.cloud.google.com/security/secret-manager) in the Google Cloud Console and add a new Version to the already existing secret "github-pat-token". Paste the PAT into the Secret value field and click "ADD NEW VERSION".

> [!TIP]
> Currently it is only possible to provide **one** PAT to the secret. That's why you can't combine an Enterprise with an Organization or Repository.

That's it 👍

As soon as you start a GitHub workflow whose job's `runs-on` labels fully satisfy at least one of the label groups configured via [`github_runner_label_groups`](./variables.tf) (default `[["self-hosted"]]`), a VM instance with the specified `machine_type` starts. The name of the VM instance starts with the `github_runner_prefix`, followed by a random string to make the name unique; the same name is the runner's name in the GitHub runner group or repository. The spawned runner registers with the **job's** full `runs-on` labels — the groups are a webhook filter, not the registered label set. After the workflow job completes, the VM instance is deleted.

> [!NOTE]
> Two label-disjoint groups (e.g. `[["spock"], ["spock-prime"]]`) let one autoscaler serve two pools without GitHub's scheduler cross-assigning runners. See [Multiple label-disjoint pools](#multiple-label-disjoint-pools) below.

## Advanced Configuration

Have a look at the [variables.tf](./variables.tf) file how to further configure the Terraform module.

This are the most common variables you may want to change:

`max_concurrency`: Select a maximum number of parallel workflow jobs to be expected (add 10% overhead).

`github_runner_label_groups`: One or more label groups the autoscaler matches against incoming workflow jobs (OR-of-ANDs — a job matches if it carries ALL labels of ANY one group). Examples: `[["self-hosted"]]` (default single-pool), `[["self-hosted", "linux"]]` (single pool, two required labels), `[["spock"], ["spock-prime"]]` (two disjoint pools served by one autoscaler).

`machine_type`: The VM instance machine type where the GitHub runner will run on by default (can be individually overwritten per workflow job, see [Magic Labels](#magic-labels))

`disk_size_gb`: The size of the VM disk


> [!TIP]
> To find the cheapest VM machine_type use this [table](https://gcloud-compute.com/instances.html) and sort by Spot instance cost. But remember that the price varies depending on the region.

## Runner features

* Executed by unprivileged user with name `agent` with the default uid `10000` and gid `10000`. Can be changed with `github_runner_uid`.
* Provides docker-daemon and docker-buildx by default. Additional packages can be installed with `github_runner_packages`.
* Only works with images that are based on debian (rely on apt package manager). Runs image `ubuntu-minimal-2204-lts` by default. Change with `machine_image`.

#### Multiple label-disjoint pools

A single autoscaler can serve multiple workflow-job populations by configuring `github_runner_label_groups` with more than one group. The autoscaler accepts a job if it matches **any** group; the spawned runner registers with the **job's** `runs-on` labels (not the group's). For pool isolation, make the groups disjoint — otherwise GitHub's scheduler may route a job into the wrong pool.

```hcl
github_runner_label_groups = [
  ["spock"],         # default-sized VMs for runs-on: spock
  ["spock-prime"],   # custom-sized VMs for runs-on: [spock-prime, gce-machine-<type>]
]
```

Per-pool defaults (disk size, image, preemptibility, runner group, max concurrency) are **not** supported — the instance template is shared across all groups. Only `machine_type` diverges, via the per-job `gce-machine-*` magic label below.

#### Magic Labels

Each workflow job can select a different machine type than the configured default `machine_type`. Use the special label `gce-machine-<type>`, e.g. `gce-machine-c2d-standard-16`. Make sure the configured `disk_type` is supported by the machine.

```yaml
jobs:
  example:
    runs-on: [self-hosted, gce-machine-c2d-standard-16]  # runs on a c2d-standard-16 VM
    steps:
      - run: echo Hello world!
```

> [!NOTE]
> Earlier versions of this module documented `@machine:<type>` (e.g. `@machine:c2d-standard-16`). That syntax does not work: GitHub's JIT runner-registration API rejects labels containing `@` or `:`, so runners spawned for such jobs could not match the job's required labels and the job timed out. Replace `@machine:<type>` with `gce-machine-<type>` in any existing workflows. Jobs still using the old syntax are detected in the webhook handler and skipped (no VM is created) with a warning logged pointing at this section.

## Expected Cost

The following Google Cloud resources are created that may generate cost:
* Cloud Task (covered by Free Tier)
* Secret Version (covered by Free Tier)
* Artifact Registry (covered by Free Tier)
* Cloud Run (covered by Free Tier)
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

Overall, only the compute instance accounts for the "majority" of the costs.

## How it works

1. As soon as a new GitHub workflow job signals a "queued" status, the GitHub webhook event "Workflow jobs" invokes the Cloud Run [container](https://github.com/Privatehive/gcp-hosted-github-runner/pkgs/container/github-runner-autoscaler) with path `/webhook`
2. The Cloud Run validates the caller source (signature) and if valid enqueues a "create-vm" Cloud task callback with a short delay (defaults to 10 seconds).
   * Edge Case for workflow jobs with a deployment review: if another GitHub webhook is received shortly afterwards indicating that the job status has been changed from "queued" to "waiting", the "create-vm" cloud task callback will be deleted (if it has not yet been processed). As soon as the deployment review for the workflow job is complete, we start again from the beginning.
   * Edge Case for canceled workflow jobs: If a workflow job is immediately canceled a GitHub webhook signals a "completed" job status. The "create-vm" cloud task callback will be deleted (if it has not yet been processed)
3. The Cloud task callback invokes the Cloud Run path `/create_vm`.
4. Cloud Run creates a jit-config (using PAT from Secret Manager). The runner is then already registered (but marked as offline).
5. The Cloud Run creates the VM instance from the instance template (preemtible spot VM instance by default) and provides it with the runner jit-config via custom metadata attribute.
6. The runner starts working on the workflow job.
7. As soon as the workflow job completed, the GitHub webhook event "Workflow jobs" invokes the Cloud Run again.
8. The Cloud Run validates the caller source (signature) and if valid enqueues a "delete-vm" Cloud task.
9.  The Cloud task invokes the Cloud Run path `/delete_vm`.
10. The Cloud Run deletes the VM instance.

> [!NOTE]
> There are webhook related error cases that can lead to duplicate VMs or missing VMs. This happens, for example, if GitHub webhooks are received twice or if webhooks are missing or the received webhooks are in the wrong order. Such errors occur rarely but cannot be completely avoided.
> To avoid unnecessary costs, any superfluous VM that does not pick a workflow job within one minute will stop (but not delete) itself.

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

The VM will stop (but not delete) itself if the registration at the GitHub runner group fails. This can be caused by:
* A typo in the GitHub Enterprise, Organization, Repository name. Check the Terraform variables `github_enterprise`, `github_organization`, `github_repositories` for typos.
* A not existing GitHub runner group within the Enterprise/Organization. Check the Terraform variable `github_runner_group` for typos.
* The GitHub runner version is [deprecated](https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/autoscaling-with-self-hosted-runners#controlling-runner-software-updates-on-self-hosted-runners). The GitHub runner won't accept any Workflow job. Check the Terraform variable `github_runner_download_url` and update to latest GitHub runner version or leave empty to always use the latest version.

You can observer the runner registration process by connecting to the VM instance via SSH (see `enable_ssh`) and running:
```
$ sudo journalctl -u google-startup-scripts.service --follow
```

#### New VM Instance not created (but a lot of instances are already running)

You exceeded your projects vCPU limit for the machine type in the region or for all regions. You may find an error log message in the Cloud Run logs stating `Machine Type vCPU quota exceeded for region`. Request a quota increase from google customer support for the project.

#### Nothing happens at all

The job's `runs-on` labels don't fully satisfy any group in `github_runner_label_groups`. Either add the missing labels to your workflow job's `runs-on`, or add a new group to the module configuration. The autoscaler logs the parsed groups at startup and the rejection reason on every miss — check Cloud Run logs for the exact mismatch.

> [!TIP]
> When bumping the module `source` ref, also bump `runner_image_tag` so the Terraform-side encoding (`;`/`,` for label groups) and the autoscaler image's parser advance together. The Cloud Run startup log renders the parsed groups in `[a, b], [c, d]` form — eyeball that after a version bump to confirm the parser saw what Terraform sent.

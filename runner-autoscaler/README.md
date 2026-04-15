# Autoscaler

#### Creates/Deletes VM instances depending on GitHub workflow jobs webhook events

A webserver is listening for GitHub "Workflow jobs" webhook events. Depending on the workflow job, a VM instance will be either created or deleted. The [10 second timeout](https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks#respond-within-10-seconds) of the GitHub webhook has to be worked around (10 sec are not enough to start VM instance) by using a Clout Task queue that calls the webserver back with an increased timeout of 180 seconds (can be configured).

### Scaling rules

> [!IMPORTANT]
> If the scaler is configured incorrectly, this can lead to “dangling” computing instances, resulting in unnecessary costs.

Following conditions of the workflow job webhook event have to be fulfilled, so a new VM instance will be **created**:

* The (enterprise, organization, repository) webhook source was configured and the webhook signature is valid (see GITHUB_ENTERPRISE, GITHUB_ORG, GITHUB_REPOS).
* The webhook `action` value equals `queued`.
* The workflow job's labels fully satisfy at least one of the label groups configured in RUNNER_LABELS (OR-of-ANDs).

Following conditions of the workflow job webhook event have to be fulfilled, so an existing VM instance will be **deleted**:

* The (enterprise, organization, repository) webhook source was configured and the webhook signature is valid (see GITHUB_ENTERPRISE, GITHUB_ORG, GITHUB_REPOS).
* The webhook `action` value equals `completed`.
* The webhook `workflow_job.runner_group_id` value equals the configured RUNNER_GROUP_ID.
* The workflow job's labels fully satisfy at least one of the label groups configured in RUNNER_LABELS.

### Configuration

The scaler is configured via the following environment variables:

| Env                     | Default                                | Description                                                                                                                                                                                                                                         |
| ----------------------- | -------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| ROUTE_WEBHOOK           | "/webhook"                             | The Cloud Run path that is invoked by the GitHub webhook. Depending on the workflow job, a Cloud Task "delete runner" or "create runner" is enqueued.                                                                                               |
| ROUTE_DELETE_VM         | "/delete_vm"                           | The Cloud Run callback path invoked by Cloud Task when a VM instance should be **deleted**. The payload contains the name of the "to be deleted" VM instance.                                                                                       |
| ROUTE_CREATE_VM         | "/create_vm"                           | The Cloud Run callback path invoked by Cloud Task when a VM instance should be **created**. The payload contains the name of the "to be created" VM instance.                                                                                       |
| PROJECT_ID              | ""                                     | The Google Cloud Project Id.                                                                                                                                                                                                                        |
| ZONES                   | "" *(comma separated list)*            | One or multiple Google Cloud zones where the VM instances will be created in. The zone is selected at random for each instance.                                                                                                                     |
| TASK_QUEUE              | ""                                     | The relative resource name of the Cloud Task queue.                                                                                                                                                                                                 |
| TASK_DISPATCH_TIMEOUT   | "180"                                  | The timeout in seconds for the Cloud Task callback (should be longer than it takes to create/delete a VM instance)                                                                                                                                  |
| CREATE_VM_DELAY         | "10"                                   | The delay in seconds to wait before the VM is created. Useful for skipping the VM creation if the workflow job is canceled by the user shortly afterwards.                                                                                          |
| INSTANCE_TEMPLATE       | ""                                     | The relative resource name of the instance template from which the VM instance will be created.                                                                                                                                                     |
| SECRET_VERSION          | ""                                     | The relative resource name of the secret version which contains the PAT or PAT classic.                                                                                                                                                             |
| RUNNER_PREFIX           | "runner"                               | Prefix for the the name of a new VM instance. A random string (10 random lower case characters) will be added to make the name unique: "<prefix>-<random_string>".                                                                                  |
| RUNNER_GROUP_ID         | "1"                                    | The GitHub runner group ID where the VM instance is expected to join as a self hosted runner.                                                                                                                                                       |
| RUNNER_LABELS           | "self-hosted"                          | One or more label groups, OR-of-ANDs. Groups are separated by `;`, labels within a group by `,`. Whitespace is trimmed per label; magic labels (`gce-machine-*`) are skipped per group. A workflow job is accepted if its labels fully satisfy at least one group. Examples: `"spock"` (one group, one label), `"spock,linux"` (one group, two required labels), `"spock;spock-prime"` (two single-label groups), `"spock,linux;spock-prime,linux"` (two two-label groups). If parsing yields zero groups every webhook is rejected and a warning is logged. |
| GITHUB_ENTERPRISE       | ""                                     | The name of the GitHub Enterprise and a webhook secret (base64 encoded) separated by ";".                                                                                                                                                           |
| GITHUB_ORG              | ""                                     | The name of the GitHub Organization and a webhook secret (base64 encoded) separated by ";".                                                                                                                                                         |
| GITHUB_REPOS            | "" *(comma separated list)*            | The GitHub repo path (USER/REPO_NAME) and a webhook secret (base64 encoded) separated by ";". Multiple repo path;secret pairs can be provided by separating them by ",". E.g. <USER>/<REPO_NAME>;<BASE64_SECRET>,<USER>/<REPO_NAME>;<BASE64_SECRET> |
| SOURCE_QUERY_PARAM_NAME | "src"                                  | The query param name that has to be present for every webhook call and must contain the webhook source name configured with GITHUB_ENTERPRISE, GITHUB_ORG, GITHUB_REPOS.                                                                            |
| PORT                    | "8080"                                 | To which port the webserver is bound.                                                                                                                                                                                                               |
| DEBUG                   | "0"                                    | Enable debug logs. Secrets may be leaked.                                                                                                                                                                                                           |
| SIMULATE                | "0"                                    | If enabled no VMs will be created - only used for development.                                                                                                                                                                                      |

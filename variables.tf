variable "machine_type" {
  type        = string
  description = "The VM instance machine type where the GitHub runner will run on."
  default     = "e2-micro"
}

variable "disk_type" {
  type        = string
  description = "The VM instance disk type"
  default     = "pd-standard"
}

variable "disk_size_gb" {
  type        = number
  description = "The VM disk size"
  default     = 40
}

variable "machine_image" {
  type        = string
  description = "The VM instance boot image (gcloud compute images list --filter ubuntu-os). Only Linux is supported!"
  default     = "ubuntu-os-cloud/ubuntu-minimal-2204-lts"
}

variable "machine_preemtible" {
  type        = bool
  description = "The VM instance will be an preemtible spot instance that costs much less but may be stopped by gcp at any time (leading to a failed workflow job)."
  default     = true
}

variable "machine_creation_delay" {
  type        = number
  description = "The creation of the VM instance is delayed by the specified number of seconds. Useful for skipping the VM creation if the workflow job is canceled by the user shortly after creation. Set the value to 0 for immediate creation with the disadvantage that VMs are created for workflow jobs that have been canceled."
  default     = 10
}

variable "max_concurrency" {
  type        = number
  description = "The estimated maximum number of concurrent workflow jobs"
  default     = 500
  validation {
    condition     = var.max_concurrency <= 1000 && var.max_concurrency > 0
    error_message = "The value must be between 0 < x <= 1000"
  }
}

variable "machine_timeout" {
  type        = number
  description = "The maximum time a VM may run. Pick a number that is well outside the expected runner job timeouts but small enough to prevent unnecessary cost if a webhook event was lost or was not processed."
  default     = 14400 // 4 h
}

variable "machine_zones" {
  type        = list(string)
  description = "One or multiple Google Cloud zones where the VM instances will be created in. The zone is selected at random for each instance."
  default     = []
}

variable "autoscaler_timeout" {
  type        = number
  description = "The timeout of the autoscaler in seconds. Should be greater than the time required to create/delete a VM instance."
  default     = 180
}

variable "enable_ssh" {
  type        = bool
  description = "Enable SSH access to the VM instances."
  default     = false
}

variable "use_cloud_nat" {
  type        = bool
  description = "Use a cloud NAT and router instead of a public ip address for the VM instances."
  default     = false
}

variable "subnet_ip_cidr_range" {
  type        = string
  description = "CIDR range to assign to subnet VM instances launch in."
  default     = "10.0.1.0/24"
}

variable "enable_debug" {
  type        = bool
  description = "Enable debug messages of github-runner-autoscaler Cloud Run (WARNING: secrets will be leaked in log files)."
  default     = false
}

variable "github_enterprise" {
  type        = string
  description = "The name of the GitHub enterprise the runner will join."
  default     = ""
}

variable "github_organization" {
  type        = string
  description = "The name of the GitHub organization the runner will join."
  default     = ""
}

variable "github_repositories" {
  type        = list(string)
  description = "The name(s) of GitHub repositories the runner will join. The format of the repository is: OWNER/REPO."
  default     = []
}

variable "github_runner_group_id" {
  type        = number
  description = "The ID of the GitHub runner group the runner will join."
  default     = 1
}

variable "github_runner_labels" {
  type        = list(string)
  description = "One or multiple labels the runner will be tagged with."
  default     = ["self-hosted"]
  validation {
    condition     = length(var.github_runner_labels) > 0
    error_message = "The variable github_runner_labels must contain at least one value!"
  }
}

variable "github_runner_prefix" {
  type        = string
  description = "The name prefix of the runner (a random string will be automatically added to make the name unique)."
  default     = "runner"
}

variable "github_runner_download_url" {
  type        = string
  description = "A download link pointing to the gitlab runner package (WARNING: deprecated runner versions won't process jobs). If this variable is empty (by default), the latest runner release will be downloaded."
  default     = ""
}

variable "github_runner_uid" {
  type        = number
  description = "The uid the runner will be run with."
  default     = 10000
}

variable "github_runner_packages" {
  type        = list(string)
  description = "Additional packages that will be installed in the runner with apt."
  default     = []
}

variable "force_cloud_run_deployment" {
  type        = bool
  description = "Use only for development: Each Terraform apply leads to a new revision of the cloud run. The module normally gates Cloud Run revisions on the resolved autoscaler image digest (see var.runner_image_tag) - use this escape hatch to force a revision regardless (e.g., after rotating a secret consumed at startup, or if the Artifact Registry remote-repo proxy is serving a stale manifest)."
  default     = false
}

variable "runner_image_tag" {
  type        = string
  description = "Docker image tag for the autoscaler, resolved against the Artifact Registry mirror of ghcr.io/commander-finance/github-runner-autoscaler. Defaults to \"master\" (floats with the latest master build - the digest-based data source ensures Cloud Run redeploys only when the underlying image digest actually changes). Pin a specific release by setting this to a date-version tag (e.g. \"26.04.14.152345\") that matches the git ref you've pinned the module to. Use \"sha-<full-commit-sha>\" for debug pins to a specific commit."
  default     = "master"
}

variable "simulate" {
  type        = bool
  description = "Use only for development: If enabled no VMs will be created/deleted."
  default     = false
}

variable "run_setup_on_runner_machines" {
  type        = bool
  description = "If true, the startup script will install required dependencies (docker.io, docker-buildx, curl, sed, jq, and any github_runner_packages) and add the 'agent' user with required permissions. Set to false if you are using a custom image that already contains all required dependencies."
  default     = true
}
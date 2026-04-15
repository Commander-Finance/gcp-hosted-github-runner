terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~>5.0"
    }
    docker = {
      source  = "kreuzwerker/docker"
      version = "~>4.0"
    }
  }
}

# Aliased provider used only by data.docker_registry_image.autoscaler to
# read the public autoscaler manifest from ghcr.io anonymously, bypassing
# any ghcr.io credentials in the consumer's Docker config.
#
# auth_disabled = true injects dummy basic-auth creds that ghcr rejects
# with a non-2xx (403 DENIED); kreuzwerker/docker >=4.0 then silently
# retries the token request without Authorization and ghcr returns an
# anonymous pull token. Requires ~>4.0 (v3.x has no retry). If
# ghcr.io/commander-finance/github-runner-autoscaler flips to private,
# drop auth_disabled and supply real credentials.
provider "docker" {
  alias = "ghcr_anonymous"
  registry_auth {
    address       = "ghcr.io"
    auth_disabled = true
  }
}

data "google_client_config" "current" {
}

data "google_project" "current" {
}

locals {
  webhookUrl           = "/webhook"
  projectId            = data.google_client_config.current.project
  projectNumber        = data.google_project.current.number
  region               = data.google_client_config.current.region
  zones                = distinct(concat(var.machine_zones, data.google_client_config.current.zone != null ? [data.google_client_config.current.zone] : []))
  runnerLabel          = join(",", var.github_runner_labels)
  hasEnterprise        = length(var.github_enterprise) > 0
  hasOrg               = length(var.github_organization) > 0
  hasRepo              = length(var.github_repositories) > 0
  sourceQueryParamName = "src"
  runnerDockerImage    = "commander-finance/github-runner-autoscaler"
  runnerDockerTag      = var.runner_image_tag

  # Single-region AR is a module invariant (dockerRepository.tf uses
  # location = local.region); multi-region would require generalizing.
  autoscaler_image_ref = "${local.region}-docker.pkg.dev/${local.projectId}/${google_artifact_registry_repository.ghcr.repository_id}/${local.runnerDockerImage}@${data.docker_registry_image.autoscaler.sha256_digest}"
}

resource "google_project_service" "compute_api" {
  service = "compute.googleapis.com"
}

resource "google_project_service" "cloud_run_api" {
  service = "run.googleapis.com"
}

resource "google_project_service" "artifactregistry_api" {
  service = "artifactregistry.googleapis.com"
}

resource "google_project_service" "cloudtasks_api" {
  service = "cloudtasks.googleapis.com"
}

resource "google_project_service" "secretmanager_api" {
  service = "secretmanager.googleapis.com"
}

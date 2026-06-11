data "docker_registry_image" "autoscaler" {
  provider = docker.ghcr_anonymous
  name     = "ghcr.io/${local.runnerDockerImage}:${local.runnerDockerTag}"
}

resource "random_password" "webhook_enterprise_secret" {
  length  = 24
  special = true
}

resource "random_password" "webhook_org_secret" {
  length  = 24
  special = true
}

resource "random_password" "webhook_repo_secret" {
  for_each = toset(var.github_repositories)
  length   = 24
  special  = true
}

resource "google_cloud_run_v2_service" "autoscaler" {
  location   = local.region
  name       = "github-runner-autoscaler"
  ingress    = "INGRESS_TRAFFIC_ALL"
  depends_on = [google_artifact_registry_repository.ghcr, google_project_service.cloud_run_api]

  template {
    service_account                  = google_service_account.autoscaler_sa.email
    max_instance_request_concurrency = var.max_concurrency
    timeout                          = format("%ds", var.autoscaler_timeout)
    scaling {
      min_instance_count = 0
      max_instance_count = 1
    }
    containers {
      image = local.autoscaler_image_ref
      env {
        name  = "ROUTE_WEBHOOK"
        value = local.webhookUrl
      }
      env {
        name  = "PROJECT_ID"
        value = local.projectId
      }
      env {
        name  = "ZONES"
        value = join(",", local.zones)
      }
      env {
        name  = "TASK_QUEUE"
        value = google_cloud_tasks_queue.autoscaler_tasks.id
      }
      env {
        name  = "TASK_DISPATCH_TIMEOUT"
        value = var.autoscaler_timeout
      }
      env {
        name  = "CREATE_VM_DELAY"
        value = var.machine_creation_delay
      }
      env {
        name  = "INSTANCE_TEMPLATE"
        value = google_compute_instance_template.runner_instance.id
      }
      env {
        # On-demand (STANDARD) fallback template, only present when the primary is
        # SPOT. Empty otherwise (the autoscaler then skips the fallback pass).
        name  = "INSTANCE_TEMPLATE_FALLBACK"
        value = var.machine_preemtible ? google_compute_instance_template.runner_instance_ondemand[0].id : ""
      }
      env {
        name  = "SECRET_VERSION"
        value = "${google_secret_manager_secret.github_pat_token.id}/versions/latest"
      }
      env {
        name  = "RUNNER_PREFIX"
        value = var.github_runner_prefix
      }
      env {
        name  = "RUNNER_GROUP_ID"
        value = var.github_runner_group_id
      }
      env {
        name  = "RUNNER_LABELS"
        value = local.runnerLabel
      }
      env {
        name  = "GITHUB_ENTERPRISE"
        value = local.hasEnterprise ? format("%s;%s", var.github_enterprise, base64encode(random_password.webhook_enterprise_secret.result)) : ""
      }
      env {
        name  = "GITHUB_ORG"
        value = local.hasOrg ? format("%s;%s", var.github_organization, base64encode(random_password.webhook_org_secret.result)) : ""
      }
      env {
        name  = "GITHUB_REPOS"
        value = local.hasRepo ? join(",", [for i, v in var.github_repositories : format("%s;%s", v, base64encode(random_password.webhook_repo_secret[v].result))]) : ""
      }
      env {
        name  = "SOURCE_QUERY_PARAM_NAME"
        value = local.sourceQueryParamName
      }
      env {
        name  = "AUTOSCALER_VERSION"
        value = local.runnerDockerTag
      }
      env {
        # Route served by the autoscaler for recreate-VM callbacks posted by
        # the per-instance shutdown script when a runner dies without accepting
        # a job (spot preemption pre-pickup, register/dispatch timeout). Kept
        # as an env var (matching ROUTE_WEBHOOK) so the route can be overridden
        # without a code change.
        name  = "ROUTE_RECREATE_VM"
        value = "/recreate_vm"
      }
      env {
        # journald log substring the shutdown script uses to decide whether this
        # runner ever accepted a workflow job. Must match the pattern the startup
        # script uses for its Phase-2 dispatch-wait (runner_job_log_pattern),
        # keeping both sides of the job-accepted check in sync via a single
        # variable.
        name  = "RUNNER_JOB_LOG_PATTERN"
        value = var.runner_job_log_pattern
      }
      dynamic "env" {
        for_each = var.force_cloud_run_deployment ? [0] : []
        content {
          name  = "TIMESTAMP"
          value = timestamp()
        }
      }
      dynamic "env" {
        for_each = var.enable_debug ? [0] : []
        content {
          name  = "DEBUG"
          value = 1
        }
      }
      dynamic "env" {
        for_each = var.simulate ? [0] : []
        content {
          name  = "SIMULATE"
          value = 1
        }
      }
      resources {
        startup_cpu_boost = false
        cpu_idle          = true
        limits = {
          cpu    = "1"
          memory = "128Mi"
        }
      }
    }
  }
}

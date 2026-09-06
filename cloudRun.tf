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
  depends_on = [google_artifact_registry_repository.ghcr, google_project_service.cloud_run_api, google_project_iam_member.runner_state, google_service_account_iam_member.enqueue_callback_identity]

  template {
    service_account                  = google_service_account.autoscaler_sa.email
    max_instance_request_concurrency = 32
    timeout                          = format("%ds", var.autoscaler_timeout)
    scaling {
      min_instance_count = 1
      max_instance_count = 3
    }
    containers {
      image = local.autoscaler_image_ref
      dynamic "env" {
        for_each = {
          STATE_DATABASE           = google_firestore_database.runner.name
          CALLBACK_BASE_URL        = local.callback_base_url
          CALLBACK_SERVICE_ACCOUNT = google_service_account.callback.email
          DELETE_TASK_QUEUE        = google_cloud_tasks_queue.delete_tasks.id
          MAINTENANCE_TASK_QUEUE   = google_cloud_tasks_queue.maintenance_tasks.id
          MAX_RUNNERS              = tostring(var.max_runners)
          MAX_ON_DEMAND_RUNNERS    = tostring(var.max_on_demand_runners)
          ALLOW_ON_DEMAND          = var.allow_on_demand ? "1" : "0"
          ALLOWED_MACHINE_TYPES    = join(" ", var.allowed_machine_types)
          DISCOVERY_REPOSITORIES   = join(" ", var.discovery_repositories)
          MACHINE_TIMEOUT          = tostring(var.machine_timeout)
          MAX_REQUEST_BYTES        = "1048576"
        }
        content {
          name  = env.key
          value = env.value
        }
      }
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
        name  = "ZONE_BENCH_MIN_VMS"
        value = var.zone_bench_min_vms
      }
      env {
        name  = "ZONE_BENCH_MIN_RATIO"
        value = var.zone_bench_min_ratio
      }
      env {
        name  = "ZONE_HEALTH_WINDOW"
        value = var.zone_health_window
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
        # Ordered, comma-separated x86-64 machine types the autoscaler tries (in region)
        # when a job has no gce-machine-* magic label and capacity is exhausted. Empty =>
        # legacy behavior (template default machine_type). All entries must be compatible
        # with the template's disk_type (e.g. Hyperdisk-balanced families).
        name  = "RUNNER_MACHINE_TYPE_FALLBACKS"
        value = join(",", var.machine_type_fallbacks)
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
        startup_cpu_boost = true
        cpu_idle          = true
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
      }
    }
  }
}

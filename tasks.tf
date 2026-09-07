resource "random_string" "task_queue_suffix" {
  length  = 5
  upper   = false
  special = false
  numeric = false
}

# One retry policy covers the create and delete queues. The autoscaler receives
# the same values (TASK_RETRY_* in cloudRun.tf) so its dispatch marker spans a
# complete retry chain; durable job records and the scheduled reconciler recover
# tasks that exhaust it. Keep retries bounded to avoid doomed hot loops.
locals {
  task_retry = {
    max_attempts        = 4
    max_retry_seconds   = 120
    min_backoff_seconds = 10
    max_backoff_seconds = 30
    max_doublings       = 2
  }
}

resource "google_cloud_tasks_queue" "autoscaler_tasks" {
  name       = "autoscaler-callback-queue-${random_string.task_queue_suffix.result}"
  location   = local.region
  depends_on = [google_project_service.cloudtasks_api]

  retry_config {
    max_attempts       = local.task_retry.max_attempts
    max_retry_duration = "${local.task_retry.max_retry_seconds}s"
    max_backoff        = "${local.task_retry.max_backoff_seconds}s"
    min_backoff        = "${local.task_retry.min_backoff_seconds}s"
    max_doublings      = local.task_retry.max_doublings
  }

  rate_limits {
    max_concurrent_dispatches = var.create_concurrency
    max_dispatches_per_second = var.create_dispatches_per_second
  }
}

# Cleanup never waits behind capacity-bound inserts.
resource "google_cloud_tasks_queue" "delete_tasks" {
  name       = "runner-delete-${random_string.task_queue_suffix.result}"
  location   = local.region
  depends_on = [google_project_service.cloudtasks_api]
  rate_limits {
    max_concurrent_dispatches = 4
    max_dispatches_per_second = 4
  }
  retry_config {
    max_attempts       = local.task_retry.max_attempts
    max_retry_duration = "${local.task_retry.max_retry_seconds}s"
    min_backoff        = "${local.task_retry.min_backoff_seconds}s"
    max_backoff        = "${local.task_retry.max_backoff_seconds}s"
    max_doublings      = local.task_retry.max_doublings
  }
}
resource "google_cloud_tasks_queue" "maintenance_tasks" {
  name       = "runner-maintenance-${random_string.task_queue_suffix.result}"
  location   = local.region
  depends_on = [google_project_service.cloudtasks_api]
  rate_limits {
    max_concurrent_dispatches = 2
    max_dispatches_per_second = 2
  }
  retry_config {
    max_attempts       = 4
    max_retry_duration = "120s"
    min_backoff        = "30s"
    max_backoff        = "300s"
    max_doublings      = 4
  }
}

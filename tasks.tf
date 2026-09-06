resource "random_string" "task_queue_suffix" {
  length  = 5
  upper   = false
  special = false
  numeric = false
}

resource "google_cloud_tasks_queue" "autoscaler_tasks" {
  name       = "autoscaler-callback-queue-${random_string.task_queue_suffix.result}"
  location   = local.region
  depends_on = [google_project_service.cloudtasks_api]

  // Durable job records and the scheduled reconciler recover tasks that exhaust
  // this queue's retry window. Keep retries bounded to avoid doomed hot loops.
  retry_config {
    max_attempts       = 16
    max_retry_duration = "7200s"
    max_backoff        = "600s"
    min_backoff        = "60s"
    max_doublings      = 4
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
    max_attempts       = 50
    max_retry_duration = "86400s"
    min_backoff        = "10s"
    max_backoff        = "600s"
    max_doublings      = 5
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
    max_attempts  = 10
    min_backoff   = "30s"
    max_backoff   = "300s"
    max_doublings = 4
  }
}

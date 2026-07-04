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

  // Capacity/quota-bound creates return HTTP 500 (no zone/family/model could place the
  // VM), so Cloud Tasks retries this callback with backoff. This window is the fleet's
  // ONLY self-heal mechanism for a job that can't get a runner: the GitHub workflow_job
  // "queued" webhook fires exactly once and never re-fires, and no reconciliation sweep
  // re-drives stranded jobs. A job still capacity-bound when retries stop strands until
  // GitHub cancels it at the 24h queue timeout.
  //
  // Both limits must be reached to stop retrying, so the effective window is the LONGER
  // of (time to exhaust max_attempts) and max_retry_duration. With min_backoff=60s,
  // max_backoff=600s, max_doublings=4, the 15 gaps before attempt 16 are
  // 60,120,240,480, then 600 x 11 = 7500s (~125 min), which governs over the 7200s
  // duration. Net: ~2h of automatic retries (one attempt per ~10 min once backoff caps).
  //
  // 2h comfortably covers every realistic *transient* crunch (the observed quota bursts
  // drain in minutes) without churning a doomed job pointlessly. It is NOT a substitute
  // for a reconciliation sweep against a *sustained* (>2h) multi-family stockout - that
  // remains the belt-and-suspenders gap, blocked on the autoscaler PAT lacking actions:read.
  retry_config {
    max_attempts       = 16
    max_retry_duration = "7200s"
    max_backoff        = "600s"
    min_backoff        = "60s"
    max_doublings      = 4
  }

  rate_limits {
    max_concurrent_dispatches = var.max_concurrency
    max_dispatches_per_second = var.max_concurrency
  }
}

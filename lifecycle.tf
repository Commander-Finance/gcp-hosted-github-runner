# Durable lifecycle state is intentionally separate from any application database.
resource "google_project_service" "firestore_api" {
  service            = "firestore.googleapis.com"
  disable_on_destroy = false
}
resource "google_project_service" "scheduler_api" {
  service            = "cloudscheduler.googleapis.com"
  disable_on_destroy = false
}
resource "google_firestore_database" "runner" {
  project         = local.projectId
  name            = "github-runners"
  location_id     = coalesce(var.firestore_location, local.region)
  type            = "FIRESTORE_NATIVE"
  deletion_policy = "ABANDON"
  depends_on      = [google_project_service.firestore_api]
}
resource "google_firestore_field" "job_expiry" {
  project    = local.projectId
  database   = google_firestore_database.runner.name
  collection = "jobs"
  field      = "expires_at"
  ttl_config {}
  index_config {}
}
resource "google_project_iam_member" "runner_state" {
  project = local.projectId
  role    = "roles/datastore.user"
  member  = "serviceAccount:${google_service_account.autoscaler_sa.email}"
  condition {
    title      = "Runner lifecycle database only"
    expression = "resource.name == 'projects/${local.projectId}/databases/${google_firestore_database.runner.name}'"
  }
}
resource "google_service_account" "callback" {
  account_id   = "runner-callback"
  display_name = "Runner task and scheduler callback identity"
}
resource "google_service_account_iam_member" "enqueue_callback_identity" {
  service_account_id = google_service_account.callback.name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.autoscaler_sa.email}"
}
locals {
  callback_base_url = "https://github-runner-autoscaler-${local.projectNumber}.${local.region}.run.app"
}
resource "google_cloud_scheduler_job" "runner_maintenance" {
  for_each         = toset(["reconcile", "sweep", "discover"])
  name             = "github-runner-${each.key}"
  region           = local.region
  schedule         = each.key == "discover" ? "*/5 * * * *" : "*/2 * * * *"
  time_zone        = "Etc/UTC"
  attempt_deadline = "${var.autoscaler_timeout}s"
  http_target {
    uri         = "${local.callback_base_url}/${each.key}"
    http_method = "POST"
    body        = base64encode("{}")
    headers     = { "Content-Type" = "application/json" }
    oidc_token {
      service_account_email = google_service_account.callback.email
      audience              = local.callback_base_url
    }
  }
  retry_config { retry_count = 3 }
  depends_on = [google_project_service.scheduler_api, google_cloud_run_v2_service.autoscaler]
}

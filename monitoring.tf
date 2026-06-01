// Observability for the autoscaler. The autoscaler had no metrics/alerting, so VM
// creation failures and the spot-vs-on-demand split were invisible. These
// log-based metrics turn the autoscaler's structured logs into metrics, plus an
// alert policy for VM creation failures.

resource "google_project_service" "monitoring_api" {
  service            = "monitoring.googleapis.com"
  disable_on_destroy = false
}

locals {
  autoscaler_log_filter = "resource.type=\"cloud_run_revision\" resource.labels.service_name=\"${google_cloud_run_v2_service.autoscaler.name}\""
}

// Counts created runner VMs, labelled by provisioning model ("spot" / "standard").
// Use this to chart the proportion of SPOT vs on-demand usage and to watch for a
// spike in on-demand fallbacks (a capacity/cost signal). The label is extracted from
// the structured "Created instance ... as <model>" log emitted by the autoscaler.
resource "google_logging_metric" "runner_vm_created" {
  name   = "github_runner/vm_created"
  filter = "${local.autoscaler_log_filter} jsonPayload.provisioning_model=(\"spot\" OR \"standard\")"

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"
    labels {
      key         = "provisioning_model"
      value_type  = "STRING"
      description = "spot or standard"
    }
  }

  label_extractors = {
    "provisioning_model" = "EXTRACT(jsonPayload.provisioning_model)"
  }
}

// Counts VM creation failures (a zone/quota sweep that exhausted all options, or a
// non-retryable create error).
resource "google_logging_metric" "runner_vm_create_failed" {
  name   = "github_runner/vm_create_failed"
  filter = "${local.autoscaler_log_filter} severity=ERROR (jsonPayload.message=~\"^Could not create instance\" OR jsonPayload.message=~\"^Exhausted all zones\")"

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"
  }
}

resource "google_monitoring_alert_policy" "runner_vm_create_failed" {
  display_name = "GitHub runner: VM creation failures"
  combiner     = "OR"
  depends_on   = [google_project_service.monitoring_api]

  conditions {
    display_name = "create-vm failures in the last 5 min"
    condition_threshold {
      filter          = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.runner_vm_create_failed.name}\" resource.type=\"cloud_run_revision\""
      comparison      = "COMPARISON_GT"
      threshold_value = 0
      duration        = "0s"
      trigger {
        count = 1
      }
      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_SUM"
      }
    }
  }

  notification_channels = var.alert_notification_channels
}

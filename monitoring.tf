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

// Counts JIT runner-registration failures (GitHub generate-jitconfig errors: 409
// name conflicts, non-201 responses, network/parse failures). These abort the
// create BEFORE the Insert, so they are NOT covered by vm_create_failed - and they
// strand the job with no runner ("stuck pending"). A spike here is the exact signal
// that went unnoticed during the 2026-06 incident, where a flood of 409 conflicts
// silently left jobs pending until a human spotted it.
resource "google_logging_metric" "runner_jit_config_failed" {
  name   = "github_runner/jit_config_failed"
  filter = "${local.autoscaler_log_filter} severity>=WARNING jsonPayload.message=~\"jit-config\""

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"
  }
}

resource "google_monitoring_alert_policy" "runner_jit_config_failed" {
  display_name = "GitHub runner: JIT runner-registration failures (jobs stuck pending)"
  combiner     = "OR"
  depends_on   = [google_project_service.monitoring_api]

  conditions {
    display_name = "jit-config failures in the last 5 min"
    condition_threshold {
      filter     = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.runner_jit_config_failed.name}\" resource.type=\"cloud_run_revision\""
      comparison = "COMPARISON_GT"
      // Baseline is ~0 (a few transient failures over weeks); a real incident is dozens
      // in minutes. >5 / 5min distinguishes a registration-failure spike from noise.
      threshold_value = 5
      duration        = "0s"
      trigger {
        count = 1
      }
      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_SUM"
        // Sum across Cloud Run revisions: failures split between an old and new
        // revision (e.g. mid-deploy) must still total against the threshold, or a
        // real spike spread thin per-revision would never cross >5.
        cross_series_reducer = "REDUCE_SUM"
        group_by_fields      = []
      }
    }
  }

  notification_channels = var.alert_notification_channels
}

// Counts Compute write-rate throttles where the autoscaler backed off instead of
// cycling families (the "returning for Cloud Tasks backoff" log). A sustained rate
// here means the Compute "Write requests per minute per region" quota is under
// pressure - the precursor to the create-amplification spiral that hangs jobs.
resource "google_logging_metric" "runner_rate_limited" {
  name   = "github_runner/rate_limited"
  filter = "${local.autoscaler_log_filter} severity>=WARNING jsonPayload.message=~\"returning for Cloud Tasks backoff\""

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"
  }
}

resource "google_monitoring_alert_policy" "runner_rate_limited" {
  display_name = "GitHub runner: Compute write-rate throttling"
  combiner     = "OR"
  depends_on   = [google_project_service.monitoring_api]

  conditions {
    display_name = "rate-limit backoffs in the last 5 min"
    condition_threshold {
      filter     = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.runner_rate_limited.name}\" resource.type=\"cloud_run_revision\""
      comparison = "COMPARISON_GT"
      // Occasional backoffs are handled gracefully; a sustained burst (>10 / 5min)
      // signals the write-rate quota is too low for current load - act before it hurts.
      threshold_value = 10
      duration        = "0s"
      trigger {
        count = 1
      }
      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_SUM"
        // Sum across Cloud Run revisions so backoffs split between revisions still
        // total against the >10 threshold (see jit_config_failed for rationale).
        cross_series_reducer = "REDUCE_SUM"
        group_by_fields      = []
      }
    }
  }

  notification_channels = var.alert_notification_channels
}

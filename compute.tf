locals {
  github_runner_package_install = join(" ", var.github_runner_packages)
}

resource "google_compute_instance_template" "runner_instance" {

  name         = "ephemeral-github-runner"
  region       = local.region
  machine_type = var.machine_type
  tags         = var.enable_ssh ? ["http-egress", "icmp-ingress", "ssh-ingress"] : ["http-egress", "icmp-ingress"]
  depends_on   = [google_project_service.compute_api]

  scheduling {
    preemptible                 = var.machine_preemtible
    automatic_restart           = false
    on_host_maintenance         = "TERMINATE"
    instance_termination_action = "DELETE"
    provisioning_model          = var.machine_preemtible ? "SPOT" : "STANDARD"

    max_run_duration {
      seconds = var.machine_timeout
    }
  }

  disk {
    auto_delete  = true
    boot         = true
    source_image = var.machine_image
    disk_type    = var.disk_type
    disk_size_gb = var.disk_size_gb
  }

  service_account {
    email  = google_service_account.github_runner_sa.email
    scopes = var.runner_service_account_scopes
  }

  network_interface {
    network    = google_compute_network.vpc_network.name
    subnetwork = google_compute_subnetwork.subnetwork.name
    nic_type   = var.runner_nic_type

    dynamic "access_config" {
      for_each = var.use_cloud_nat ? [] : [0]
      content {
        network_tier = "STANDARD"
      }
    }
  }
}

// On-demand (STANDARD) counterpart of the runner template, created only when the
// primary template is preemptible/SPOT. The autoscaler falls back to this template
// when SPOT capacity is exhausted in every zone, so a stockout no longer hangs the
// job. Kept as a separate Terraform-managed template (rather than overriding
// scheduling at Insert time) because max_run_duration must be preserved and the
// compute Go SDK in use cannot set it. Identical to runner_instance except for the
// scheduling block.
resource "google_compute_instance_template" "runner_instance_ondemand" {

  count = var.machine_preemtible ? 1 : 0

  name         = "ephemeral-github-runner-ondemand"
  region       = local.region
  machine_type = var.machine_type
  tags         = var.enable_ssh ? ["http-egress", "icmp-ingress", "ssh-ingress"] : ["http-egress", "icmp-ingress"]
  depends_on   = [google_project_service.compute_api]

  scheduling {
    preemptible         = false
    automatic_restart   = false
    on_host_maintenance = "TERMINATE"
    // Valid with provisioning_model=STANDARD because max_run_duration is set
    // (instance_termination_action requires SPOT or a limited-run instance, and a
    // VM with max_run_duration qualifies). DELETE keeps the ephemeral-runner
    // semantics consistent with the SPOT template.
    instance_termination_action = "DELETE"
    provisioning_model          = "STANDARD"

    max_run_duration {
      seconds = var.machine_timeout
    }
  }

  disk {
    auto_delete  = true
    boot         = true
    source_image = var.machine_image
    disk_type    = var.disk_type
    disk_size_gb = var.disk_size_gb
  }

  service_account {
    email  = google_service_account.github_runner_sa.email
    scopes = var.runner_service_account_scopes
  }

  network_interface {
    network    = google_compute_network.vpc_network.name
    subnetwork = google_compute_subnetwork.subnetwork.name
    nic_type   = var.runner_nic_type

    dynamic "access_config" {
      for_each = var.use_cloud_nat ? [] : [0]
      content {
        network_tier = "STANDARD"
      }
    }
  }
}

// First parameter has to be the registration token
/*
resource "google_compute_project_metadata_item" "startup_scripts_register_runner" {
  key   = "startup_script_register_runner"
  value = <<EOT
#!/bin/bash
echo "Setup of agent '$(hostname)' started"
apt-get update && apt-get -y install docker.io docker-buildx curl
useradd -d /home/agent -u ${var.github_runner_uid} agent
usermod -aG docker agent
newgrp docker
curl -s -o /tmp/agent.tar.gz -L '${var.github_runner_download_url}'
mkdir -p /home/agent
chown -R agent:agent /home/agent
pushd /home/agent
sudo -u agent tar zxf /tmp/agent.tar.gz
registration_token=$1
sudo -u agent ./config.sh --unattended --disableupdate --ephemeral --name $(hostname) ${local.runnerLabelInstanceTemplate} --url 'https://github.com/${var.github_organization}' --token $${registration_token} --runnergroup '${var.github_runner_group_name}' || shutdown now
./bin/installdependencies.sh || shutdown now
./svc.sh install agent || shutdown now
./svc.sh start || shutdown now
popd
rm /tmp/agent.tar.gz
echo "Setup finished"
EOT
}*/


locals {
  # Harden the kernel against security-sensitive ICMP behaviors. The VPC firewall
  # allows all ICMP ingress so that off-path "fragmentation needed" (Type 3 Code 4)
  # replies reach the runner and Path MTU Discovery keeps working; this sysctl
  # block drops the dangerous bits (redirects, broadcast amplification, source
  # routing, bogus error responses) at the kernel level instead.
  icmp_hardening_subscript = <<EOT
cat >/etc/sysctl.d/99-runner-icmp-hardening.conf <<'SYSCTL' || shutdown now
net.ipv4.conf.all.accept_redirects=0
net.ipv4.conf.default.accept_redirects=0
net.ipv4.conf.all.secure_redirects=0
net.ipv4.conf.default.secure_redirects=0
net.ipv4.conf.all.send_redirects=0
net.ipv4.conf.default.send_redirects=0
net.ipv4.conf.all.accept_source_route=0
net.ipv4.conf.default.accept_source_route=0
net.ipv4.icmp_echo_ignore_broadcasts=1
net.ipv4.icmp_ignore_bogus_error_responses=1
net.ipv6.conf.all.accept_redirects=0
net.ipv6.conf.default.accept_redirects=0
net.ipv6.conf.all.accept_source_route=0
net.ipv6.conf.default.accept_source_route=0
SYSCTL
sysctl --load=/etc/sysctl.d/99-runner-icmp-hardening.conf >/dev/null || shutdown now
EOT

  # Define the setup and install subscript that should run if we are using a default base image, such as the default ubuntu-os-cloud/ubuntu-minimal-2204-lts.
  # Transient network steps (apt, runner download) are wrapped in retry() (defined in
  # the parent startup script) so a single apt/curl hiccup doesn't abandon the job.
  setup_and_install_subscript = <<EOT
retry ${var.runner_setup_retries} 5 bash -c 'apt-get update && DEBIAN_FRONTEND=noninteractive apt-get -y install docker.io docker-buildx curl sed jq ${local.github_runner_package_install}' || shutdown now
useradd -d /home/agent -u ${var.github_runner_uid} agent
usermod -aG docker agent
newgrp docker
RUNNER_DOWNLOAD_URL='${var.github_runner_download_url}'
if [ -z "$${RUNNER_DOWNLOAD_URL}" ]; then
  RUNNER_VERSION=$(curl -s "https://github.com/actions/runner/tags/" | grep -Eo "$Version v[0-9]+.[0-9]+.[0-9]+" | sort -r | head -n1 | tr -d ' ' | tr -d 'v')
  echo "Downloading latest runner v$${RUNNER_VERSION}"
  RUNNER_DOWNLOAD_URL="https://github.com/actions/runner/releases/download/v$${RUNNER_VERSION}/actions-runner-linux-x64-$${RUNNER_VERSION}.tar.gz"
fi
retry ${var.runner_setup_retries} 5 curl -fsS -o /tmp/agent.tar.gz -L "$${RUNNER_DOWNLOAD_URL}" || shutdown now
mkdir -p /home/agent
chown -R agent:agent /home/agent
pushd /home/agent
sudo -u agent tar zxf /tmp/agent.tar.gz
popd
rm /tmp/agent.tar.gz
EOT
}

// First parameter has to be the base64 encoded jit_config
resource "google_compute_project_metadata_item" "startup_scripts_register_jit_runner" {
  key   = "startup_script_register_jit_runner"
  value = <<EOT
#!/bin/bash
agent_name=$(hostname)
echo "Setup of agent '$agent_name' started"

# retry <attempts> <initial_delay_seconds> <command...>
# Retries a command with escalating backoff to absorb transient apt/curl/network
# failures during setup instead of abandoning the job on the first hiccup.
retry() {
  local attempts=$1; shift
  local delay=$1; shift
  local n=1
  until "$@"; do
    if [ $n -ge $attempts ]; then
      echo "Command failed after $n attempt(s): $*"
      return 1
    fi
    echo "Attempt $n/$attempts failed: $* - retrying in $${delay}s"
    sleep $delay
    delay=$((delay * 3))
    n=$((n + 1))
  done
  return 0
}

${local.icmp_hardening_subscript}
${var.run_setup_on_runner_machines ? local.setup_and_install_subscript : ""}

if [ ! -d /home/agent ]; then
  echo "ERROR: /home/agent directory does not exist. When using a custom image, ensure the runner is pre-installed at /home/agent."
  shutdown now
fi
cd /home/agent

encoded_jit_config=$1
echo -n $encoded_jit_config | base64 -d | jq '.".runner"' -r | base64 -d > .runner
echo -n $encoded_jit_config | base64 -d | jq '.".credentials"' -r | base64 -d > .credentials
echo -n $encoded_jit_config | base64 -d | jq '.".credentials_rsaparams"' -r | base64 -d > .credentials_rsaparams
sed -i 's/{{SvcNameVar}}/actions.runner.service/g' bin/systemd.svc.sh.template
sed -i 's/{{SvcDescription}}/GitHub Actions Runner/g' bin/systemd.svc.sh.template
cp bin/systemd.svc.sh.template ./svc.sh && chmod +x ./svc.sh
retry ${var.runner_setup_retries} 5 ./bin/installdependencies.sh || shutdown now
./svc.sh install agent || shutdown now
./svc.sh start || shutdown now

echo "Setup finished - waiting for the runner to come online"
register_timeout=${var.runner_register_timeout}
dispatch_timeout=${var.runner_job_dispatch_timeout}
online_pattern='${var.runner_online_log_pattern}'
job_pattern='${var.runner_job_log_pattern}'

runner_log() { journalctl -u actions.runner.service --no-pager; }

# Phase 1: wait (briefly) for the runner to register / come online. A runner that
# never comes online will never run a job, so we give up quickly here.
elapsed=0
online=0
while [ $elapsed -lt $register_timeout ]; do
  if runner_log | grep -q "$job_pattern"; then
    echo "Accepted Workflow Job - processing"
    exit 0
  fi
  if runner_log | grep -q "$online_pattern"; then
    echo "Runner is online - waiting for a job to be dispatched"
    online=1
    break
  fi
  sleep 5
  elapsed=$((elapsed + 5))
done

if [ $online -eq 0 ]; then
  echo "Runner did not come online within $${register_timeout}s - shutting down"
  shutdown now
fi

# Phase 2: the runner is online and healthy; wait (generously) for GitHub to
# dispatch the job. The previous hard 180s window shut down healthy runners while
# their job was still queued, hanging the job.
elapsed=0
while [ $elapsed -lt $dispatch_timeout ]; do
  if runner_log | grep -q "$job_pattern"; then
    echo "Accepted Workflow Job - processing"
    exit 0
  fi
  sleep 5
  elapsed=$((elapsed + 5))
done

echo "No job dispatched within $${dispatch_timeout}s - shutting down"
shutdown now
EOT
}

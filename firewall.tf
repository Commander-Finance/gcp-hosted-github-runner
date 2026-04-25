resource "google_compute_firewall" "http_egress" {
  name    = "http-egress"
  description = "Allows egress on port 80, 443"
  network = google_compute_network.vpc_network.name
  direction = "EGRESS"

  allow {
    protocol = "tcp"
    ports    = ["80", "443"]
  }

  target_tags = ["http-egress"]
}

resource "google_compute_firewall" "icmp_ingress" {
  name        = "icmp-ingress"
  description = "Allow ICMP so off-path 'fragmentation needed' (Type 3 Code 4) replies reach the runner, keeping Path MTU Discovery working on STANDARD-tier egress"
  network     = google_compute_network.vpc_network.name
  direction   = "INGRESS"

  allow {
    protocol = "icmp"
  }

  source_ranges = ["0.0.0.0/0"]
  target_tags   = ["icmp-ingress"]
}

resource "google_compute_firewall" "ssh_ingress" {
  count   = var.enable_ssh ? 1 : 0
  name    = "ssh-ingress"
  description = "Allows ingress on port 22"
  network = google_compute_network.vpc_network.name
  direction = "INGRESS"

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  source_ranges = ["0.0.0.0/0"]
  target_tags = ["ssh-ingress"]
}

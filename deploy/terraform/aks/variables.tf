# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0

variable "subscription_id" {
  type        = string
  description = "Azure subscription to deploy into."
}

variable "name" {
  type        = string
  default     = "nbsandbox"
  description = "Prefix for every resource name (lowercase letters and digits)."
  validation {
    condition     = can(regex("^[a-z][a-z0-9]{2,11}$", var.name))
    error_message = "3-12 lowercase letters and digits, starting with a letter."
  }
}

variable "location" {
  type    = string
  default = "southeastasia"
}

variable "network_policy" {
  type        = string
  default     = "cilium"
  description = <<-EOT
    What enforces NetworkPolicy, which run Pods rely on: "cilium" (Azure CNI
    powered by Cilium), "calico", or "azure" (Azure Network Policy Manager).
    nbsandbox works with any of them; workers check it with a probe Pod.
  EOT
  validation {
    condition     = contains(["cilium", "calico", "azure"], var.network_policy)
    error_message = "cilium, calico or azure."
  }
}

variable "kubernetes_version" {
  type        = string
  default     = null
  description = "AKS version; null: the region's default."
}

variable "system_vm_size" {
  type    = string
  default = "Standard_D4s_v5"
}

variable "runs_vm_size" {
  type        = string
  default     = "Standard_D8s_v5"
  description = "Nodes that run notebooks (each run gets runs_memory)."
}

variable "runs_min_nodes" {
  type    = number
  default = 1
}

variable "runs_max_nodes" {
  type    = number
  default = 5
}

variable "pod_sandboxing" {
  type        = bool
  default     = false
  description = <<-EOT
    Run notebooks in Kata VMs (AKS Pod Sandboxing): each run gets its own
    kernel. Needs the Azure Linux OS SKU and a supported VM size; check the
    current AKS documentation for availability in your region.
  EOT
}

variable "nbsandbox_image" {
  type    = string
  default = "ghcr.io/azbyluthfan/nbsandboxd:0.1.0"
}

variable "runner_image" {
  type    = string
  default = "ghcr.io/azbyluthfan/nbsandbox-runner:0.1.0"
}

variable "api_replicas" {
  type    = number
  default = 2
}

variable "worker_replicas" {
  type    = number
  default = 2
}

variable "worker_concurrency" {
  type    = number
  default = 4
}

variable "runs_memory" {
  type    = string
  default = "2GiB"
}

variable "oidc_issuer" {
  type        = string
  description = "Your identity provider's issuer, e.g. https://login.microsoftonline.com/<tenant id>/v2.0 (docs/auth.md)."
}

variable "oidc_audience" {
  type        = string
  description = "nbsandbox's app registration (client) ID: tokens must be issued for it."
}

variable "oidc_id_prefix" {
  type    = string
  default = "entra"
}

variable "oidc_id_claims" {
  type    = list(string)
  default = ["tid", "oid"]
}

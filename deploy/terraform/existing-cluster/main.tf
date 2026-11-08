# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# nbsandbox on a cluster you already run (on premises, or any cloud), with
# a PostgreSQL database and S3-compatible storage you already have. The
# cluster's network plugin must enforce NetworkPolicy (Cilium, Calico, ...):
# workers refuse to run notebooks if their probe can reach anything.

terraform {
  required_version = ">= 1.6"
  required_providers {
    helm       = { source = "hashicorp/helm", version = "~> 2.17" }
    kubernetes = { source = "hashicorp/kubernetes", version = "~> 2.35" }
  }
}

variable "kubeconfig" {
  type    = string
  default = "~/.kube/config"
}

variable "kube_context" {
  type    = string
  default = null
}

variable "database_url" {
  type        = string
  sensitive   = true
  description = "postgres://user:password@host:5432/nbsandbox?sslmode=require"
}

variable "s3_endpoint" {
  type        = string
  description = "host[:port] of the S3-compatible store (RustFS, Ceph RGW, ...), no scheme."
}

variable "s3_bucket" {
  type    = string
  default = "nbsandbox"
}

variable "s3_tls" {
  type    = bool
  default = true
}

variable "s3_access_key" {
  type      = string
  sensitive = true
}

variable "s3_secret_key" {
  type      = string
  sensitive = true
}

variable "nbsandbox_image" {
  type    = string
  default = "ghcr.io/azbyluthfan/nbsandboxd:0.1.0"
}

variable "runner_image" {
  type    = string
  default = "ghcr.io/azbyluthfan/nbsandbox-runner:0.1.0"
}

variable "runtime_class" {
  type        = string
  default     = ""
  description = "Optional RuntimeClass for run Pods (kata, gvisor); it must exist on the run nodes."
}

variable "runs_node_selector" {
  type    = map(string)
  default = {}
}

variable "oidc_issuer" {
  type = string
}

variable "oidc_audience" {
  type = string
}

variable "oidc_id_prefix" {
  type    = string
  default = "oidc"
}

variable "oidc_id_claims" {
  type    = list(string)
  default = ["sub"]
}

provider "kubernetes" {
  config_path    = var.kubeconfig
  config_context = var.kube_context
}

provider "helm" {
  kubernetes {
    config_path    = var.kubeconfig
    config_context = var.kube_context
  }
}

resource "kubernetes_namespace" "nbsandbox" {
  metadata { name = "nbsandbox" }
}

resource "kubernetes_secret" "database" {
  metadata {
    name      = "nbsandbox-db"
    namespace = kubernetes_namespace.nbsandbox.metadata[0].name
  }
  data = { "database-url" = var.database_url }
}

resource "kubernetes_secret" "s3" {
  metadata {
    name      = "nbsandbox-s3"
    namespace = kubernetes_namespace.nbsandbox.metadata[0].name
  }
  data = { "access-key" = var.s3_access_key, "secret-key" = var.s3_secret_key }
}

resource "helm_release" "nbsandbox" {
  name      = "nbsandbox"
  namespace = kubernetes_namespace.nbsandbox.metadata[0].name
  chart     = "${path.module}/../../helm/nbsandbox"

  values = [yamlencode({
    image       = { repository = split(":", var.nbsandbox_image)[0], tag = split(":", var.nbsandbox_image)[1] }
    runnerImage = var.runner_image
    runs        = { runtimeClass = var.runtime_class, nodeSelector = var.runs_node_selector }
    database    = { existingSecret = kubernetes_secret.database.metadata[0].name }
    storage = {
      kind = "s3"
      s3   = { endpoint = var.s3_endpoint, bucket = var.s3_bucket, tls = var.s3_tls, existingSecret = kubernetes_secret.s3.metadata[0].name }
    }
    auth = {
      basic = false
      oidc  = { issuer = var.oidc_issuer, audience = var.oidc_audience, idPrefix = var.oidc_id_prefix, idClaims = var.oidc_id_claims }
    }
  })]
}

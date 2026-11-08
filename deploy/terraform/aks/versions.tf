# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0

terraform {
  required_version = ">= 1.6"
  required_providers {
    azurerm    = { source = "hashicorp/azurerm", version = "~> 4.14" }
    helm       = { source = "hashicorp/helm", version = "~> 2.17" }
    kubernetes = { source = "hashicorp/kubernetes", version = "~> 2.35" }
    random     = { source = "hashicorp/random", version = "~> 3.6" }
  }
}

provider "azurerm" {
  features {}
  subscription_id     = var.subscription_id
  storage_use_azuread = true # the storage account has no shared keys
}

provider "kubernetes" {
  host                   = azurerm_kubernetes_cluster.this.kube_config[0].host
  client_certificate     = base64decode(azurerm_kubernetes_cluster.this.kube_config[0].client_certificate)
  client_key             = base64decode(azurerm_kubernetes_cluster.this.kube_config[0].client_key)
  cluster_ca_certificate = base64decode(azurerm_kubernetes_cluster.this.kube_config[0].cluster_ca_certificate)
}

provider "helm" {
  kubernetes {
    host                   = azurerm_kubernetes_cluster.this.kube_config[0].host
    client_certificate     = base64decode(azurerm_kubernetes_cluster.this.kube_config[0].client_certificate)
    client_key             = base64decode(azurerm_kubernetes_cluster.this.kube_config[0].client_key)
    cluster_ca_certificate = base64decode(azurerm_kubernetes_cluster.this.kube_config[0].cluster_ca_certificate)
  }
}

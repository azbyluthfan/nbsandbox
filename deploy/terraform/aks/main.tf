# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
#
# nbsandbox on AKS:
# - an AKS cluster with a NetworkPolicy engine (Cilium by default; Calico or
#   Azure NPM also work: run Pods rely on the policy, whoever enforces it),
#   Workload Identity, and a separate, tainted node pool for runs;
# - PostgreSQL Flexible Server, reachable only from the cluster's network;
# - a storage account with no shared keys: nbsandbox reaches it with its
#   own managed identity (Workload Identity, no secrets);
# - the Helm chart.

data "azurerm_client_config" "current" {}

resource "azurerm_resource_group" "this" {
  name     = "${var.name}-rg"
  location = var.location
}

# ---------------------------------------------------------------- network

resource "azurerm_virtual_network" "this" {
  name                = "${var.name}-vnet"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  address_space       = ["10.40.0.0/16"]
}

resource "azurerm_subnet" "aks" {
  name                 = "aks"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = ["10.40.0.0/20"]
}

resource "azurerm_subnet" "postgres" {
  name                 = "postgres"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = ["10.40.16.0/24"]
  delegation {
    name = "postgres"
    service_delegation {
      name    = "Microsoft.DBforPostgreSQL/flexibleServers"
      actions = ["Microsoft.Network/virtualNetworks/subnets/join/action"]
    }
  }
}

resource "azurerm_private_dns_zone" "postgres" {
  name                = "${var.name}.private.postgres.database.azure.com"
  resource_group_name = azurerm_resource_group.this.name
}

resource "azurerm_private_dns_zone_virtual_network_link" "postgres" {
  name                  = "${var.name}-postgres"
  resource_group_name   = azurerm_resource_group.this.name
  private_dns_zone_name = azurerm_private_dns_zone.postgres.name
  virtual_network_id    = azurerm_virtual_network.this.id
}

# ---------------------------------------------------------------- cluster

resource "azurerm_kubernetes_cluster" "this" {
  name                      = "${var.name}-aks"
  location                  = azurerm_resource_group.this.location
  resource_group_name       = azurerm_resource_group.this.name
  dns_prefix                = var.name
  kubernetes_version        = var.kubernetes_version
  oidc_issuer_enabled       = true
  workload_identity_enabled = true

  default_node_pool {
    name                         = "system"
    vm_size                      = var.system_vm_size
    node_count                   = 2
    vnet_subnet_id               = azurerm_subnet.aks.id
    only_critical_addons_enabled = false # the API processes and workers run here
    upgrade_settings { max_surge = "1" }
  }

  identity { type = "SystemAssigned" }

  network_profile {
    # Azure CNI with a NetworkPolicy engine (any enforcing one works; the
    # Cilium data plane needs the Cilium policy engine).
    network_plugin      = "azure"
    network_plugin_mode = "overlay"
    network_data_plane  = var.network_policy == "cilium" ? "cilium" : "azure"
    network_policy      = var.network_policy
  }
}

# Notebooks run here only: the taint keeps everything else off, and the run
# Pods tolerate it. It scales with the queue.
resource "azurerm_kubernetes_cluster_node_pool" "runs" {
  name                  = "runs"
  kubernetes_cluster_id = azurerm_kubernetes_cluster.this.id
  vm_size               = var.runs_vm_size
  vnet_subnet_id        = azurerm_subnet.aks.id
  auto_scaling_enabled  = true
  min_count             = var.runs_min_nodes
  max_count             = var.runs_max_nodes
  node_labels           = { "nbsandbox.io/runs" = "true" }
  node_taints           = ["nbsandbox.io/runs=true:NoSchedule"]
  # Pod Sandboxing: each run in its own Kata VM (runtime class below).
  os_sku           = var.pod_sandboxing ? "AzureLinux" : null
  workload_runtime = var.pod_sandboxing ? "KataVmIsolation" : null
  # A fork bomb in a notebook stops at this many processes per Pod.
  kubelet_config { pod_max_pid = 300 }
  upgrade_settings { max_surge = "1" }
}

# ---------------------------------------------------------------- database

resource "random_password" "postgres" {
  length  = 32
  special = false
}

resource "azurerm_postgresql_flexible_server" "this" {
  name                          = "${var.name}-pg"
  resource_group_name           = azurerm_resource_group.this.name
  location                      = azurerm_resource_group.this.location
  version                       = "17"
  sku_name                      = "GP_Standard_D2ds_v5"
  storage_mb                    = 65536
  backup_retention_days         = 14
  delegated_subnet_id           = azurerm_subnet.postgres.id
  private_dns_zone_id           = azurerm_private_dns_zone.postgres.id
  public_network_access_enabled = false
  administrator_login           = "nbsandbox"
  administrator_password        = random_password.postgres.result
  zone                          = "1"
  depends_on                    = [azurerm_private_dns_zone_virtual_network_link.postgres]
}

resource "azurerm_postgresql_flexible_server_database" "nbsandbox" {
  name      = "nbsandbox"
  server_id = azurerm_postgresql_flexible_server.this.id
}

# ---------------------------------------------------------------- storage

resource "azurerm_storage_account" "this" {
  name                            = "${var.name}${substr(md5(azurerm_resource_group.this.id), 0, 8)}"
  resource_group_name             = azurerm_resource_group.this.name
  location                        = azurerm_resource_group.this.location
  account_tier                    = "Standard"
  account_replication_type        = "ZRS"
  shared_access_key_enabled       = false # identities only
  allow_nested_items_to_be_public = false
  min_tls_version                 = "TLS1_2"
  blob_properties {
    versioning_enabled = true
    delete_retention_policy { days = 7 }
  }
}

resource "azurerm_storage_container" "nbsandbox" {
  name                  = "nbsandbox"
  storage_account_id    = azurerm_storage_account.this.id
  container_access_type = "private"
}

# nbsandbox's identity: the API processes and workers use it (Workload
# Identity) to read and write blobs. No keys anywhere.
resource "azurerm_user_assigned_identity" "nbsandbox" {
  name                = "${var.name}-identity"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
}

resource "azurerm_role_assignment" "blobs" {
  scope                = azurerm_storage_account.this.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = azurerm_user_assigned_identity.nbsandbox.principal_id
}

resource "azurerm_federated_identity_credential" "nbsandbox" {
  for_each            = toset(["api", "worker"])
  name                = "nbsandbox-${each.key}"
  resource_group_name = azurerm_resource_group.this.name
  parent_id           = azurerm_user_assigned_identity.nbsandbox.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = azurerm_kubernetes_cluster.this.oidc_issuer_url
  subject             = "system:serviceaccount:nbsandbox:nbsandbox-${each.key}"
}

# ---------------------------------------------------------------- nbsandbox

resource "kubernetes_namespace" "nbsandbox" {
  metadata { name = "nbsandbox" }
}

resource "kubernetes_secret" "database" {
  metadata {
    name      = "nbsandbox-db"
    namespace = kubernetes_namespace.nbsandbox.metadata[0].name
  }
  data = {
    "database-url" = "postgres://nbsandbox:${random_password.postgres.result}@${azurerm_postgresql_flexible_server.this.fqdn}:5432/nbsandbox?sslmode=require"
  }
}

locals {
  image_repo = split(":", var.nbsandbox_image)[0]
  image_tag  = split(":", var.nbsandbox_image)[1]
}

resource "helm_release" "nbsandbox" {
  name      = "nbsandbox"
  namespace = kubernetes_namespace.nbsandbox.metadata[0].name
  chart     = "${path.module}/../../helm/nbsandbox"

  values = [yamlencode({
    image       = { repository = local.image_repo, tag = local.image_tag }
    runnerImage = var.runner_image
    api         = { replicas = var.api_replicas }
    worker      = { replicas = var.worker_replicas, concurrency = var.worker_concurrency, scratchSize = "16Gi" }
    runs = {
      namespace    = "nbsandbox-runs"
      runtimeClass = var.pod_sandboxing ? "kata-vm-isolation" : ""
      memory       = var.runs_memory
      nodeSelector = { "nbsandbox.io/runs" = "true" }
      tolerations  = [{ key = "nbsandbox.io/runs", operator = "Exists", effect = "NoSchedule" }]
    }
    database = { existingSecret = kubernetes_secret.database.metadata[0].name }
    storage = {
      kind  = "azure"
      azure = { accountURL = azurerm_storage_account.this.primary_blob_endpoint, container = azurerm_storage_container.nbsandbox.name }
    }
    serviceAccount = { annotations = { "azure.workload.identity/client-id" = azurerm_user_assigned_identity.nbsandbox.client_id } }
    podLabels      = { "azure.workload.identity/use" = "true" }
    auth = {
      basic = false
      oidc  = { issuer = var.oidc_issuer, audience = var.oidc_audience, idPrefix = var.oidc_id_prefix, idClaims = var.oidc_id_claims }
    }
  })]

  depends_on = [azurerm_role_assignment.blobs, azurerm_federated_identity_credential.nbsandbox, azurerm_kubernetes_cluster_node_pool.runs]
}

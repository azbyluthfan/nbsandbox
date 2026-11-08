# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0

output "cluster_name" {
  value = azurerm_kubernetes_cluster.this.name
}

output "resource_group" {
  value = azurerm_resource_group.this.name
}

output "kubeconfig_command" {
  value = "az aks get-credentials -g ${azurerm_resource_group.this.name} -n ${azurerm_kubernetes_cluster.this.name}"
}

output "storage_account" {
  value = azurerm_storage_account.this.name
}

output "api_service" {
  value       = "nbsandbox.nbsandbox.svc:8080"
  description = "The API inside the cluster. Expose it with an ingress that terminates TLS (chart value ingress.*)."
}

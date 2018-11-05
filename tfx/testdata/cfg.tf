provider "azurerm" {
  version                     = ">= 1.7.0"
  skip_credentials_validation = true
  skip_provider_registration  = true
}

resource "azurerm_resource_group" "rg1" {
  name     = "rg1"
  location = "eastus"
}

resource "azurerm_resource_group" "rg2" {
  name     = "rg2"
  location = "eastus"
}

resource "azurerm_virtual_network" "vnet" {
  name                = "vnet"
  location            = "${azurerm_resource_group.rg1.location}"
  resource_group_name = "${azurerm_resource_group.rg1.name}"
  address_space       = ["10.0.0.0/8"]
}

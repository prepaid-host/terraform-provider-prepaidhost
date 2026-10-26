terraform {
  required_providers {
    prepaidhost = {
      source = "prepaid-host/prepaidhost"
    }
  }
}

# The token comes from the PREPAIDHOST_TOKEN environment variable. Do not put
# it into the configuration: it would end up in version control.
provider "prepaidhost" {}

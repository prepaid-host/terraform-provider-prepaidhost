variable "contact_id" {
  description = "Id of a prepaidhost_domain_contact."
  type        = number
}

# The zone of a domain of the account. An existing domain can be imported, see
# the prepaidhost_domain resource.
resource "prepaidhost_domain" "main" {
  name       = "example.de"
  contact_id = var.contact_id
  max_price  = 15
}

resource "prepaidhost_dns_record" "www" {
  domain_id = tonumber(prepaidhost_domain.main.id)
  type      = "A"
  host      = "www"
  record    = "203.0.113.10"
  ttl       = 300
}

resource "prepaidhost_dns_record" "mail" {
  domain_id = tonumber(prepaidhost_domain.main.id)
  type      = "MX"
  host      = "@"
  record    = "mail.example.net"
  priority  = 10
}

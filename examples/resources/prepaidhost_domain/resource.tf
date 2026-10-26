resource "prepaidhost_domain_contact" "owner" {
  type      = "PERS"
  sex       = "MALE"
  firstname = "Max"
  lastname  = "Mustermann"
  street    = "Musterstrasse"
  number    = "1"
  postcode  = "12345"
  city      = "Berlin"
  country   = "DE"
  phone     = "+49.3012345678"
  email     = "max@example.com"
}

resource "prepaidhost_domain" "main" {
  name            = "example.de"
  duration_months = 12
  contact_id      = tonumber(prepaidhost_domain_contact.owner.id)

  # Creating this resource charges the balance (the price of the whole term).
  max_price = 15

  nameservers = ["ns1.example.net", "ns2.example.net"]
}

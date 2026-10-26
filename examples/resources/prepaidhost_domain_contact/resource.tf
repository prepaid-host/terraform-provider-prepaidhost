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

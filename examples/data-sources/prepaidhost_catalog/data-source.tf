# Orderable root packets with their prices, durations and OS templates.
data "prepaidhost_catalog" "packets" {
  type = "rootpacket"
}

output "packets" {
  value = data.prepaidhost_catalog.packets.packets
}

output "templates" {
  value = data.prepaidhost_catalog.packets.templates
}

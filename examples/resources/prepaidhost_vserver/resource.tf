resource "prepaidhost_ssh_key" "deploy" {
  name       = "deploy"
  public_key = file("~/.ssh/id_ed25519.pub")
}

# A root packet. Look up packet_id and template_id with the
# prepaidhost_catalog data source (type "rootpacket").
resource "prepaidhost_vserver" "web" {
  hostname        = "web1"
  packet_id       = 2
  duration_months = 1
  template_id     = 12
  ssh_key_ids     = [tonumber(prepaidhost_ssh_key.deploy.id)]

  # Creating this resource charges the balance. Refuse any order above 20 EUR.
  max_price = 20
}

# A custom server instead of a packet: cores, memory (MB), disk (GB),
# addresses, backups, traffic and duration_days come from the catalog
# (type "rootserver").
resource "prepaidhost_vserver" "custom" {
  hostname      = "worker1"
  cores         = 2
  memory        = 2048
  disk          = 40
  addresses     = 1
  backups       = 1
  traffic       = "unlimited"
  duration_days = 30
  template_id   = 12
  max_price     = 30
}

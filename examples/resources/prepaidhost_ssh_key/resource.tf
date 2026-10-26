resource "prepaidhost_ssh_key" "deploy" {
  name       = "deploy"
  public_key = file("~/.ssh/id_ed25519.pub")
}

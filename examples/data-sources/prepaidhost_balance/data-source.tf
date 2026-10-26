data "prepaidhost_balance" "current" {}

# Needs a token with the account:billing scope. Top the balance up in the
# customer panel; the provider never does that.
output "balance" {
  value = "${data.prepaidhost_balance.current.amount} ${data.prepaidhost_balance.current.currency}"
}

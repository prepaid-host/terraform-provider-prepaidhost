# A vServer ordered in the customer panel needs an import before Terraform manages it.
# The import id is the numeric id of the server; afterwards set the purchase
# arguments in the configuration to what was bought (they cannot all be read back).
terraform import prepaidhost_vserver.web 1234

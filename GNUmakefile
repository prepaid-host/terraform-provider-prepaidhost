default: fmt lint install

# Terraform/OpenTofu binary for acceptance-style tests (they run against the
# in-process mock API, never the real one).
TF_ACC_TERRAFORM_PATH ?= $(shell command -v tofu || command -v terraform)

build:
	go build -v ./...

install: build
	go install -v ./...

lint:
	golangci-lint run

fmt:
	gofmt -s -w -e .

test:
	go test -v -cover -timeout=120s -parallel=10 ./...

testacc:
	TF_ACC=1 TF_ACC_TERRAFORM_PATH=$(TF_ACC_TERRAFORM_PATH) TF_ACC_PROVIDER_NAMESPACE=prepaid-host \
		go test -v -cover -timeout 120m ./...

.PHONY: fmt lint test testacc build install

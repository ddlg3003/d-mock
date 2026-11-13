.PHONY: build test e2e e2e-checkout e2e-containers e2e-all

build:
	go build -o bin/dmock ./cmd/dmock

test:
	go test ./...

# Each target runs one example suite with `dmock test`: up, every scenario, always down.
e2e: build
	bin/dmock test examples/orders

e2e-checkout: build
	bin/dmock test examples/checkout

# containers + load (needs docker/podman and k6)
e2e-containers: build
	bin/dmock test examples/orders/containers
	bin/dmock test examples/checkout/pod-limits

e2e-all: e2e e2e-checkout e2e-containers

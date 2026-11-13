.PHONY: build test e2e e2e-checkout

build:
	go build -o bin/dmock ./cmd/dmock

test:
	go test ./...

# e2e brings the example stack up, runs its scenarios and always takes it down again.
e2e: build
	cd examples/orders && ../../bin/dmock up && \
	  (../../bin/dmock run scenarios; rc=$$?; ../../bin/dmock down; exit $$rc)

# e2e-checkout runs use case 1 of examples/checkout (MySQL in a container, the fake provider, the toy service).
e2e-checkout: build
	cd examples/checkout && ../../bin/dmock up && \
	  (../../bin/dmock run scenarios; rc=$$?; ../../bin/dmock down; exit $$rc)

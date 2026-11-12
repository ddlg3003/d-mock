.PHONY: build test e2e

build:
	go build -o bin/dmock ./cmd/dmock

test:
	go test ./...

# e2e brings the example stack up, runs its scenarios and always takes it down again.
e2e: build
	cd examples/orders && ../../bin/dmock up && \
	  (../../bin/dmock run scenarios; rc=$$?; ../../bin/dmock down; exit $$rc)

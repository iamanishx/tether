.PHONY: all build test vet fmt run clean local-doctor local-up local-status local-down local-purge e2e

all: vet test build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -o bin/tetherd ./cmd/tetherd

test:
	go test -race -count=1 ./...
	python3 -m unittest discover -s scripts -p 'test_*.py'

vet:
	go vet ./...

fmt:
	gofmt -w .

run: build
	./bin/tetherd -data-dir ./data

local-doctor:
	python3 scripts/local.py doctor

local-up:
	python3 scripts/local.py up

local-status:
	python3 scripts/local.py status

local-down:
	python3 scripts/local.py down

local-purge:
	python3 scripts/local.py down --purge

e2e: local-up
	python3 scripts/local.py smoke

clean:
	rm -rf bin data

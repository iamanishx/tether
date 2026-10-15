.PHONY: all build test vet fmt run clean

all: vet test build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -o bin/tetherd ./cmd/tetherd

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

run: build
	./bin/tetherd -data-dir ./data

clean:
	rm -rf bin data

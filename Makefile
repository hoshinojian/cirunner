.PHONY: build test vet fmt

build:
	go build -o bin/cirunner ./cmd/cirunner

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

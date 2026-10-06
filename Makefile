BINARY := niclane
GO ?= go

.PHONY: build test race vet fmt install clean

build:
	$(GO) build -o $(BINARY) ./cmd/niclane

test:
	$(GO) test -count=1 ./...

race:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

install:
	$(GO) install ./cmd/niclane

clean:
	rm -f $(BINARY)

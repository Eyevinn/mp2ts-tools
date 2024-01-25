.PHONY: all
all: test check coverage build

.PHONY: build
build: mp2ts-info mp2ts-nallister mp2ts-pslister

.PHONY: prepare
prepare:
	go mod tidy

mp2ts-info mp2ts-nallister mp2ts-pslister:
	go build -ldflags "-X github.com/Eyevinn/mp2ts-tools/internal.commitVersion=$$(git describe --tags HEAD) -X github.com/Eyevinn/mp2ts-tools/internal.commitDate=$$(git log -1 --format=%ct)" -o out/$@ ./cmd/$@/main.go

.PHONY: test
test: prepare
	go test ./...

.PHONY: coverage
coverage:
	# Ignore (allow) packages without any tests
	go test ./... -coverprofile coverage.out
	go tool cover -html=coverage.out -o coverage.html
	go tool cover -func coverage.out -o coverage.txt
	tail -1 coverage.txt

.PHONY: check
check: prepare
	golangci-lint run

.PHONY: update
update:
	go get -t -u ./...

clean:
	rm -f out/*

install: all
	cp out/* $(GOPATH)/bin/


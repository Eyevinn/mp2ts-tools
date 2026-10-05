.PHONY: all
all: test check coverage build

CMDS = mp2ts-info mp2ts-nallister mp2ts-pslister mp2ts-extract mp2ts-timeshift mp2ts-pidfilter mp2ts-loop
BINARIES = $(addprefix out/,$(CMDS))

.PHONY: build
build: $(BINARIES)

.PHONY: prepare
prepare:
	go mod tidy

# Binaries are built as packages, not as main.go files, so that they carry the
# version Go embeds from the git tag and commit (see internal/buildinfo.go).
# They are .PHONY because that version is not a file prerequisite: a binary
# built before a commit or a tag would be kept, still naming the old one. The
# build cache makes the rebuild cheap. `make <tool>` still works as a shorthand.
.PHONY: $(BINARIES) $(CMDS)
$(BINARIES): out/%:
	go build -o $@ ./cmd/$*

$(CMDS): %: out/%

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

install:
	go install $(addprefix ./cmd/,$(CMDS))


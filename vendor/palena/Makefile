.PHONY: build test lint cover image run compose-up compose-down clean

GO       ?= go
LINT     ?= golangci-lint
BIN      := bin/server
PKG      := ./...
COVER    := coverage.out
COVER_MIN ?= 80

build:
	CGO_ENABLED=0 $(GO) build -o $(BIN) ./cmd/server

test:
	$(GO) test -race -count=1 $(PKG)

lint:
	$(LINT) run

cover:
	$(GO) test -race -count=1 -coverprofile=$(COVER) $(PKG)
	@$(GO) tool cover -func=$(COVER) | awk '/^total:/ { \
	  total=int($$3); \
	  print "total coverage:", $$3; \
	  if (total < $(COVER_MIN)) { \
	    print "FAIL: coverage below $(COVER_MIN)%"; exit 1 \
	  } \
	}'

image:
	docker build -t ghcr.io/palenaai/palena-litellm-pseudonymizer:dev -f deploy/Dockerfile .

compose-up:
	docker compose up -d

compose-down:
	docker compose down -v

run: build compose-up
	@echo "Waiting for dependencies..."
	@sleep 3
	PALENA_PSEUDONYMIZER_HTTP_ADDR=:8080 \
	PALENA_PSEUDONYMIZER_REDIS_URL=redis://localhost:6379/0 \
	PALENA_PSEUDONYMIZER_PRESIDIO_ANALYZER_URL=http://localhost:5001 \
	PALENA_PSEUDONYMIZER_PRESIDIO_IMAGE_REDACTOR_URL=http://localhost:5003 \
	$(BIN)

clean:
	rm -rf bin $(COVER)

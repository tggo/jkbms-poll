# Convenience targets. Reads .env if present.
ifneq (,$(wildcard ./.env))
include .env
export
endif

BIN      ?= jkbms-poll
GOOS     ?= linux
GOARCH   ?= arm64

.PHONY: help test build build-host deploy run clean

help:
	@echo "Targets:"
	@echo "  test         go test ./..."
	@echo "  build        cross-compile $(BIN)-$(GOOS)-$(GOARCH) (default: linux/arm64)"
	@echo "  build-host   build $(BIN) for the local OS/arch"
	@echo "  deploy       scp the linux/arm64 binary to \$$DEPLOY_HOST:\$$DEPLOY_PATH"
	@echo "  run          ssh into \$$DEPLOY_HOST and run with -log debug"
	@echo "  clean        remove built binaries"
	@echo ""
	@echo "Set vars in .env (see .env.example) or override on the command line:"
	@echo "  make build GOARCH=amd64"
	@echo "  make run JKBMS_MAC=AA:BB:CC:DD:EE:FF"

test:
	go test -v ./...

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags='-s -w' \
	    -o $(BIN)-$(GOOS)-$(GOARCH) ./...

build-host:
	go build -o $(BIN) ./...

deploy: build
	@: $${DEPLOY_HOST:?set DEPLOY_HOST in .env}
	@: $${DEPLOY_PATH:?set DEPLOY_PATH in .env}
	scp $(BIN)-$(GOOS)-$(GOARCH) $(DEPLOY_HOST):$(DEPLOY_PATH)
	ssh $(DEPLOY_HOST) chmod +x $(DEPLOY_PATH)

run:
	@: $${DEPLOY_HOST:?set DEPLOY_HOST in .env}
	@: $${DEPLOY_PATH:?set DEPLOY_PATH in .env}
	@: $${JKBMS_MAC:?set JKBMS_MAC in .env}
	ssh $(DEPLOY_HOST) "JKBMS_MAC=$(JKBMS_MAC) JKBMS_CELLS=$(JKBMS_CELLS) JKBMS_OUT=$(JKBMS_OUT) $(DEPLOY_PATH) -log debug"

clean:
	rm -f $(BIN) $(BIN)-*

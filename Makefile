BINARY := bin/ac
AGENT  := bin/ac-agent
# Target hardware for the AP side: XR1710G is airoha/an7581, always arm64.
AGENT_GOARCH ?= arm64
AGENT_GOOS   ?= linux

# Public Ed25519 key for a site-specific firmware image.
ifneq ($(strip $(AC_CONTROLLER_PUBKEY)),)
AGENT_LDFLAGS := -X github.com/naoki66/ac-controller/agent.FirmwareControllerKey=$(AC_CONTROLLER_PUBKEY)
endif

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X github.com/naoki66/ac-controller/shared/version.Version=$(VERSION) \
	-X github.com/naoki66/ac-controller/shared/version.Commit=$(COMMIT) \
	-X github.com/naoki66/ac-controller/shared/version.Date=$(DATE)

.PHONY: all build run test vet fmt docker up up-dev down clean agent agent-local agent-check

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./controller/cmd/ac

# Cross-compiled AP agent. CGO is off, so one static binary covers any arm64
# OpenWrt build regardless of libc.
agent:
	CGO_ENABLED=0 GOOS=$(AGENT_GOOS) GOARCH=$(AGENT_GOARCH) \
		go build -trimpath -ldflags "$(LDFLAGS) $(AGENT_LDFLAGS)" -o $(AGENT)-$(AGENT_GOOS)-$(AGENT_GOARCH) ./agent/cmd/acagent
	@ls -lh $(AGENT)-$(AGENT_GOOS)-$(AGENT_GOARCH)

# Host build of the agent: used for dry runs against a local controller.
agent-local:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS) $(AGENT_LDFLAGS)" -o $(AGENT) ./agent/cmd/acagent

# Rehearse a revision without touching this machine's /etc: renders, verifies
# and stages only. Requires a reachable controller and root-owned flag.
agent-check:
	$(AGENT) once --dry-run -v

run:
	go run ./controller/cmd/ac serve -v

test:
	go test ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

docker:
	docker build -t ac-controller:dev -f controller/Dockerfile .

up:
	cd controller && docker compose up -d --build

# 本地联调用：端口映射形态，供 Docker Desktop（Windows/macOS）访问 Web UI 与 API。
up-dev:
	cd controller && docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build

down:
	cd controller && docker compose down

clean:
	rm -rf bin

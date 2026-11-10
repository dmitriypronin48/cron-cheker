BIN_DIR := bin
AGENT := $(BIN_DIR)/cron-checker-agent
SERVER := $(BIN_DIR)/cron-checker-server
VERSION := 0.1.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build build-linux-amd64 build-linux-arm64 test vet clean package-deb package-rpm docker-image

all: build

build: vet test $(AGENT) $(SERVER)

$(AGENT):
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(AGENT) ./cmd/agent

$(SERVER):
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(SERVER) ./cmd/server

build-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/cron-checker-agent-linux-amd64 ./cmd/agent
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/cron-checker-server-linux-amd64 ./cmd/server

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/cron-checker-agent-linux-arm64 ./cmd/agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/cron-checker-server-linux-arm64 ./cmd/server

test:
	go test ./...

vet:
	go vet ./...

package-deb: build-linux-amd64
	nfpm package -f deploy/nfpm.yaml -p deb -t $(BIN_DIR)/

package-rpm: build-linux-amd64
	nfpm package -f deploy/nfpm.yaml -p rpm -t $(BIN_DIR)/

docker-image:
	docker build -f deploy/Dockerfile -t cron-checker-server:$(VERSION) -t cron-checker-server:latest .

clean:
	rm -rf $(BIN_DIR)

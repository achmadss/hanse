BIN := bin

CMDS := hanse hanse-gateway hanse-node
PLATFORMS := linux-amd64 linux-arm64 windows-amd64 windows-arm64 darwin-amd64 darwin-arm64

.PHONY: build test lint cross clean

build:
	@mkdir -p $(BIN)
	@for cmd in $(CMDS); do \
		go build -o $(BIN)/$$cmd ./cmd/$$cmd || exit 1; \
	done

test:
	go test ./...

lint:
	go vet ./...
	golangci-lint run

cross:
	@for platform in $(PLATFORMS); do \
		os=$${platform%-*}; \
		arch=$${platform#*-}; \
		for cmd in $(CMDS); do \
			ext=; [ $$os = windows ] && ext=.exe; \
			GOOS=$$os GOARCH=$$arch go build -o $(BIN)/$$platform/$$cmd$$ext ./cmd/$$cmd || exit 1; \
		done; \
	done

clean:
	rm -rf $(BIN)

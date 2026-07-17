# cm（claude-master-go）の Makefile と同型（VERSION ldflags・完全静的ビルド）。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PKG     := ./cmd/herdr-drover
BIN     := herdr-drover

.PHONY: build test vet dist install clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

# 配布物（Release 発行用）。asset 名 herdr-drover_<os>_<arch> は install.sh /
# scripts/build.sh の将来 DL 経路と一致させること。Windows は out-of-scope
# （DESIGN: direct attach 非対応）なので darwin/linux のみ。
dist:
	rm -rf dist && mkdir -p dist
	@for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
	  os=$${t%/*}; arch=$${t#*/}; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build -trimpath -ldflags '$(LDFLAGS)' \
	    -o dist/herdr-drover_$${os}_$${arch} $(PKG) || exit 1; \
	  echo "built dist/herdr-drover_$${os}_$${arch}"; \
	done
	cd dist && shasum -a 256 herdr-drover_* > checksums.txt && cat checksums.txt

# ローカルの launchd 常駐まで一括。実 launchctl を実行する＝カットオーバー作業
# （テストは `herdr-drover install --no-launchctl` を使う。Makefile からは呼ばない）。
install: build
	./$(BIN) install

clean:
	rm -rf dist bin $(BIN)

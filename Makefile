VERSION ?= 0.1.0
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS_ENV := GOPROXY=$(GOPROXY_URL) GOSUMDB=sum.golang.google.cn CGO_ENABLED=0

# proxy.golang.org 在本机不可达（DNS 超时），用国内镜像。
GOPROXY_URL ?= https://goproxy.cn,direct

.PHONY: build all test vet selftest clean

build:
	$(GOFLAGS_ENV) go build -trimpath -ldflags="$(LDFLAGS)" -o dist/qqmail-mcp .

# 交叉编译：Windows 二进制直接在 macOS 上产出，目标机不需要 Node/Python。
all:
	$(GOFLAGS_ENV) GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/qqmail-mcp-windows-amd64.exe .
	$(GOFLAGS_ENV) GOOS=windows GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/qqmail-mcp-windows-arm64.exe .
	$(GOFLAGS_ENV) GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/qqmail-mcp-darwin-arm64 .
	$(GOFLAGS_ENV) GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/qqmail-mcp-darwin-amd64 .
	$(GOFLAGS_ENV) GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/qqmail-mcp-linux-amd64 .

vet:
	$(GOFLAGS_ENV) go vet ./...

test:
	$(GOFLAGS_ENV) go test ./...

# 需要先 export QQMAIL_USER / QQMAIL_PASSWORD
selftest: build
	./dist/qqmail-mcp -selftest

clean:
	rm -rf dist

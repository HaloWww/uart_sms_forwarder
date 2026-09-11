.PHONY: build-web build-server build-servers build-linux build-windows build-release clean dev run

# 变量定义
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X github.com/dushixiang/uart_sms_forwarder/internal/version.Version=$(VERSION)
GOFLAGS := CGO_ENABLED=0

# 构建前端
build-web:
	@echo "Building web frontend..."
	cd web && yarn && yarn build
	@echo "Web frontend built successfully!"

# 构建服务端（单平台 - Linux amd64）
build-server:
	@echo "Building server for Linux amd64..."
	@mkdir -p bin
	$(GOFLAGS) GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-linux-amd64 ./cmd/serv
	upx bin/uart_sms_forwarder-linux-amd64
	@echo "Server built successfully!"
	@ls -lh bin/

# 构建服务端（多平台）
build-servers:
	@echo "Building servers for multiple platforms..."
	@mkdir -p bin

	# Linux
	@echo "Building for Linux amd64..."
	$(GOFLAGS) GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-linux-amd64 ./cmd/serv

	@echo "Building for Linux arm64..."
	$(GOFLAGS) GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-linux-arm64 ./cmd/serv

	@echo "Building for Linux arm..."
	$(GOFLAGS) GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-linux-arm ./cmd/serv

	# Windows
	@echo "Building for Windows amd64..."
	$(GOFLAGS) GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-windows-amd64.exe ./cmd/serv

	@echo "Building for Windows arm64..."
	$(GOFLAGS) GOOS=windows GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-windows-arm64.exe ./cmd/serv

	# macOS
	@echo "Building for macOS amd64..."
	$(GOFLAGS) GOOS=darwin GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-darwin-amd64 ./cmd/serv

	@echo "Building for macOS arm64..."
	$(GOFLAGS) GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-darwin-arm64 ./cmd/serv

	# FreeBSD
	@echo "Building for FreeBSD amd64..."
	$(GOFLAGS) GOOS=freebsd GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-freebsd-amd64 ./cmd/serv

	@echo "Compressing binaries..."
	upx bin/uart_sms_forwarder-linux-* bin/uart_sms_forwarder-windows-* bin/uart_sms_forwarder-freebsd-* 2>/dev/null || true

	@echo "All servers built successfully!"
	@ls -lh bin/

# 构建 Linux 平台（用于 Docker 镜像）
build-linux:
	@echo "Building for Linux platforms (Docker)..."
	@mkdir -p bin

	# Linux amd64
	@echo "Building for Linux amd64..."
	$(GOFLAGS) GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-linux-amd64 ./cmd/serv
	upx bin/uart_sms_forwarder-linux-amd64

	# Linux arm64
	@echo "Building for Linux arm64..."
	$(GOFLAGS) GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-linux-arm64 ./cmd/serv
	upx bin/uart_sms_forwarder-linux-arm64

	@echo "Linux binaries built successfully!"
	@ls -lh bin/

# 构建 Windows 平台（无需 CGO，可在 Linux/macOS 上交叉编译）
build-windows:
	@echo "Building for Windows platforms..."
	@mkdir -p bin
	$(GOFLAGS) GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-windows-amd64.exe ./cmd/serv
	$(GOFLAGS) GOOS=windows GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o bin/uart_sms_forwarder-windows-arm64.exe ./cmd/serv
	@echo "Windows binaries built successfully!"
	@ls -lh bin/uart_sms_forwarder-windows-*.exe

# 构建所有（发布版本）
build-release:
	@echo "Building release version..."
	make build-web
	make build-server
	@echo "Release build completed!"

# 清理构建文件
clean:
	@echo "Cleaning build files..."
	rm -rf bin
	rm -rf web/dist
	@echo "Clean completed!"

# 开发构建（不包含前端，不压缩）
dev:
	@echo "Building for development..."
	@mkdir -p bin
	go build -o bin/uart_sms_forwarder ./cmd/serv
	@echo "Development build completed!"
	@ls -lh bin/

# 运行（开发模式）
run:
	@echo "Running in development mode..."
	go run ./cmd/serv

# 默认目标
build: build-release

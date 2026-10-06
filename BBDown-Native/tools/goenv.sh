#!/usr/bin/env bash
# BBDown-Native 构建环境。
# 用法：source tools/goenv.sh
#
# 说明：
#   - Go 工具链放在 .workbuddy/binaries/go/ 下（不污染系统）。
#   - 本机**没有 MSVC / gcc**，所以必须 CGO_ENABLED=0 走纯 Go 编译链。
#     好在 lxn/walk 是基于 syscall 的纯 Go 实现，不需要 cgo。
#   - goproxy.cn 是唯一可达的模块代理（proxy.golang.org 在本机不可达）。

export GOROOT="C:\\Users\\Admin\\.workbuddy\\binaries\\go\\1.26.8\\go"
export GOPATH="C:\\Users\\Admin\\.workbuddy\\binaries\\go\\path"
export GOMODCACHE="C:\\Users\\Admin\\.workbuddy\\binaries\\go\\path\\pkg\\mod"
export GOCACHE="C:\\Users\\Admin\\.workbuddy\\binaries\\go\\cache"
export GOPROXY="https://goproxy.cn,direct"
export GOFLAGS="-mod=mod"
export CGO_ENABLED=0
export GOOS=windows
export GOARCH=amd64

export PATH="/c/Users/Admin/.workbuddy/binaries/go/1.26.8/go/bin:/c/Users/Admin/.workbuddy/binaries/PortableGit/versions/1.2.0/usr/bin:/c/Windows/System32:/c/Windows:$PATH"

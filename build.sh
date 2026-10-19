#!/bin/sh
# Cross-compile static binaries for common OpenWrt targets into ./dist
# usage: ./build.sh            (all)   |   ./build.sh arm64
set -e
cd "$(dirname "$0")"
mkdir -p dist
build() { # name GOARCH [extra env]
	echo "-> $1"
	env CGO_ENABLED=0 GOOS=linux GOARCH=$2 $3 go build -trimpath -ldflags "-s -w" -o "dist/panje-$1" .
}
want=${1:-all}
[ $want = all -o $want = amd64 ]  && build amd64 amd64
[ $want = all -o $want = arm64 ]  && build arm64 arm64
[ $want = all -o $want = armv7 ]  && build armv7 arm GOARM=7
[ $want = all -o $want = mipsle ] && build mipsle mipsle GOMIPS=softfloat
[ $want = all -o $want = mips ]   && build mips mips GOMIPS=softfloat
ls -lh dist

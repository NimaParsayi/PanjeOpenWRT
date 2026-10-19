#!/bin/sh
# usage: ./install.sh <router-ip> <arch: amd64|arm64|armv7|mipsle|mips>
set -e
R=$1; A=$2; [ -n "$R" ] && [ -n "$A" ] || { echo "usage: $0 <router-ip> <arch>"; exit 1; }
cd "$(dirname "$0")"
[ -f dist/panje-$A ] || ./build.sh $A
scp -O dist/panje-$A root@$R:/usr/bin/panje
scp -O files/etc/init.d/panje root@$R:/etc/init.d/panje
ssh root@$R 'chmod +x /usr/bin/panje /etc/init.d/panje
  if command -v opkg >/dev/null; then opkg update && opkg install nftables kmod-nft-tproxy ip-full; else apk add nftables kmod-nft-tproxy ip-full; fi
  /etc/init.d/panje enable && /etc/init.d/panje restart; sleep 2; logread -e panje | tail -5'

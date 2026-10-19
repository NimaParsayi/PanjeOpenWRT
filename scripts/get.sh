#!/bin/sh
# Panje one-line installer for OpenWrt (run ON the router, as root).
#   wget -qO- https://raw.githubusercontent.com/NimaParsayi/PanjeOpenWRT/main/scripts/get.sh | sh
# Optional env: PANJE_URL=<direct url of the binary>  (e.g. a mirror)
#               PANJE_BIN=<local file>                (binary you copied to the router yourself)
#               PANJE_PASS=<panel password>           (default: random)
set -e
REPO="NimaParsayi/PanjeOpenWRT"

[ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
[ -f /etc/openwrt_release ] || { echo "this is not OpenWrt"; exit 1; }
. /etc/openwrt_release

case "$DISTRIB_ARCH" in
  aarch64*)        A=arm64 ;;
  x86_64)          A=amd64 ;;
  arm_cortex-a*)   A=armv7 ;;
  mipsel_*)        A=mipsle ;;
  mips_*|mips64_*) A=mips ;;
  *) echo "unsupported architecture: $DISTRIB_ARCH (open an issue)"; exit 1 ;;
esac
echo "==> architecture: $DISTRIB_ARCH -> panje-$A"

URL="${PANJE_URL:-https://github.com/$REPO/releases/latest/download/panje-$A}"

echo "==> installing dependencies (nftables, tproxy, ip-full, ca-bundle)"
if command -v apk >/dev/null 2>&1; then
  apk update >/dev/null && apk add nftables kmod-nft-tproxy ip-full ca-bundle
else
  opkg update >/dev/null && opkg install nftables kmod-nft-tproxy ip-full ca-bundle
fi

TMP=/tmp/panje.new
rm -f "$TMP"
if [ -n "$PANJE_BIN" ]; then
  echo "==> using local file $PANJE_BIN"
  cp "$PANJE_BIN" "$TMP"
else
  echo "==> downloading $URL"
  if command -v uclient-fetch >/dev/null 2>&1; then uclient-fetch -q -O "$TMP" "$URL"; else wget -q -O "$TMP" "$URL"; fi
fi
[ "$(wc -c < "$TMP")" -gt 5000000 ] || { echo "download failed or file too small"; exit 1; }

/etc/init.d/panje stop >/dev/null 2>&1 || true
mv "$TMP" /usr/bin/panje
chmod +x /usr/bin/panje

cat > /etc/init.d/panje <<'INIT'
#!/bin/sh /etc/rc.common
START=95
STOP=10
USE_PROCD=1

start_service() {
	procd_open_instance
	procd_set_param command /usr/bin/panje -c /etc/panje/config.json
	procd_set_param respawn 3600 5 0
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_close_instance
}

stop_service() {
	nft delete table inet panje 2>/dev/null
}
INIT
chmod +x /etc/init.d/panje

mkdir -p /etc/panje
PASS="${PANJE_PASS:-$(head -c 24 /dev/urandom | md5sum | cut -c1-12)}"
/usr/bin/panje -c /etc/panje/config.json passwd "$PASS" >/dev/null

/etc/init.d/panje enable
/etc/init.d/panje start
sleep 2

IP=$(uci -q get network.lan.ipaddr | cut -d/ -f1)
echo
echo "=============================================="
echo " Panje installed"
echo " Panel    : http://${IP:-<router-ip>}:9090"
echo " Password : $PASS"
echo "=============================================="
echo "Next: open the panel -> add a subscription -> Menu > Settings > Update geo data -> Start."

#!/bin/bash
# Capture an O2 Cloud session (with oauth_bundle) from the desktop app via mitmproxy,
# then apply it to the rclone o2native remote.
set -euo pipefail

BACKEND_DIR="$HOME/rclone-o2/backend/o2"
CAPTURE_SCRIPT="$BACKEND_DIR/o2_reauth_capture.py"
WORK_DIR="${1:-$HOME/o2-capture}"
O2_APP="/Applications/O2 Cloud.app"

mkdir -p "$WORK_DIR"
cd "$WORK_DIR"

if ! command -v mitmdump >/dev/null 2>&1; then
  echo "mitmproxy no esta instalado. Ejecuta: brew install mitmproxy" >&2
  exit 1
fi

if [ ! -d "$O2_APP" ]; then
  echo "ERROR: la app O2 Cloud no esta instalada en $O2_APP. Instalala primero." >&2
  exit 1
fi

echo "1) Arrancando mitmdump en el puerto 8080 (genera el CA de mitmproxy la primera vez)..."
mitmdump -q --set ssl_insecure=true -p 8080 -s "$CAPTURE_SCRIPT" >"$WORK_DIR/mitmdump.log" 2>&1 &
MITM_PID=$!
trap 'kill $MITM_PID 2>/dev/null || true' EXIT
sleep 3

CA="$HOME/.mitmproxy/mitmproxy-ca-cert.pem"
if [ ! -f "$CA" ]; then
  echo "No se encontro $CA; revisa $WORK_DIR/mitmdump.log" >&2
  kill $MITM_PID 2>/dev/null || true
  exit 1
fi

echo "2) La primera vez debes confiar el CA de mitmproxy (pedira sudo). Ejecuta en OTRA terminal:"
echo "   sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain $CA"
echo
echo "   Si la app O2 Cloud ya estaba abierta, cierrala antes de continuar."
read -r -p "   Cuando el CA este confiado y la app instalada, pulsa ENTER para abrirla con el proxy... " _unused

EXEC=$(find "$O2_APP/Contents/MacOS" -maxdepth 1 -type f | head -1)
if [ -z "$EXEC" ]; then
  echo "ERROR: no se encontro el ejecutable de la app O2 Cloud." >&2
  kill $MITM_PID 2>/dev/null || true
  exit 1
fi

echo "3) Abriendo la app O2 Cloud a traves del proxy: $EXEC"
https_proxy=http://127.0.0.1:8080 http_proxy=http://127.0.0.1:8080 "$EXEC" &
APP_PID=$!

echo "4) Completa el login (DNI/contraseña + SMS) en la app."
echo "   Esperando a que se capture la sesion en $WORK_DIR/o2_session.json ..."
for i in $(seq 1 240); do
  if [ -s "$WORK_DIR/o2_session.json" ]; then
    break
  fi
  sleep 2
done

if [ ! -s "$WORK_DIR/o2_session.json" ]; then
  echo "No se capturo sesion en 8 minutos." >&2
  exit 1
fi

kill $APP_PID 2>/dev/null || true
echo "5) Sesion capturada. Aplicando a rclone.conf..."
python3 "$BACKEND_DIR/apply_o2_session.py" "$WORK_DIR/o2_session.json" o2native

echo
echo "6) Comprobando con rclone (debe funcionar sin SMS):"
"$HOME/.local/bin/rclone" lsd o2native: --timeout 30s | head -10
echo "Hecho. El keepalive mantendra la sesion renovada automaticamente."

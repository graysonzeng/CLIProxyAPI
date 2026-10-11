#!/usr/bin/env bash
# Public verification of ompcli stack from the box. Key is read on the host only, never printed.
set -uo pipefail
c(){ curl -s -o /dev/null -w '%{http_code}' -m 15 "$@"; }
echo "healthz: $(curl -s -m 10 https://c.688663.xyz/healthz)"
echo "management.html: $(c https://c.688663.xyz/management.html)"
echo "k.688663.xyz: $(c https://k.688663.xyz/)"
echo "v1/models(with key, via public URL from host): $(ssh -o BatchMode=yes ompcli 'K=$(python3 -c "import yaml;print(yaml.safe_load(open(\"/root/cliproxyapi/config.yaml\"))[\"api-keys\"][0])" 2>/dev/null || awk "/^api-keys:/{f=1;next} f&&/^ *- /{gsub(/[\"\x27 -]/,\"\");print;exit}" /root/cliproxyapi/config.yaml); curl -s -o /tmp/m.json -w "%{http_code}" -m 15 -H "Authorization: Bearer $K" https://c.688663.xyz/v1/models; echo " models=$(python3 -c "import json;print(len(json.load(open(\"/tmp/m.json\"))[\"data\"]))" 2>/dev/null)"; rm -f /tmp/m.json')"
echo "deployed: $(ssh -o BatchMode=yes ompcli '/root/cliproxyapi/cli-proxy-api -version 2>&1 | head -1')"
echo "cpamp: $(ssh -o BatchMode=yes ompcli 'systemctl is-active cpa-manager-plus; systemctl show -p ExecStart --value cpa-manager-plus | grep -o "v[0-9.]*_linux" | head -1')"

#!/usr/bin/env python3
"""Reads the fresh O2 session from the gateway's session store."""
import os
import sys

GATEWAY_DIR = os.path.expanduser("~/o2cloud-gateway")
os.chdir(GATEWAY_DIR)
sys.path.insert(0, os.path.join(GATEWAY_DIR, "src"))

from o2gateway.o2.session import O2SessionStore
from o2gateway.settings import get_settings

s = get_settings()
store = O2SessionStore(s)
sess = store.read()
if not sess:
    print("NONE")
else:
    cookies = {c.name: c.value for c in sess.cookies}
    print(sess.validation_key)
    print(cookies.get("JSESSIONID", ""))

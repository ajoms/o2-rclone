#!/usr/bin/env python3
"""Apply a captured O2 session (o2_session.json) to the rclone o2native remote.

Usage:
    python3 apply_o2_session.py [session.json] [remote]
Defaults: session.json = o2_session.json (cwd), remote = o2native
"""
import configparser
import json
import os
import shutil
import sys
import time

HOME = os.path.expanduser("~")
RCLONE_CONF = os.path.join(HOME, ".config/rclone/rclone.conf")


def main():
    session_path = sys.argv[1] if len(sys.argv) > 1 else "o2_session.json"
    remote = sys.argv[2] if len(sys.argv) > 2 else "o2native"

    if not os.path.exists(session_path):
        print("session file not found: %s" % session_path)
        return 1
    with open(session_path) as f:
        s = json.load(f)

    vk = s.get("validationKey") or s.get("validation_key") or ""
    bundle = s.get("oauthBundle") or s.get("oauth_bundle") or ""
    if not vk or not bundle:
        print("session file is missing validationKey or oauthBundle; not applying")
        return 1

    cp = configparser.ConfigParser()
    cp.read(RCLONE_CONF)
    if remote not in cp:
        print("remote %s not found in %s" % (remote, RCLONE_CONF))
        return 2

    bak = RCLONE_CONF + ".bak-" + time.strftime("%Y%m%d%H%M%S")
    shutil.copy2(RCLONE_CONF, bak)
    print("rclone.conf backed up to %s" % bak)

    sec = cp[remote]
    sec["validation_key"] = vk
    sec["oauth_bundle"] = bundle
    if s.get("jsessionid"):
        sec["cookie_jsessionid"] = s["jsessionid"]
    if s.get("deviceId"):
        sec["device_id"] = s["deviceId"]
    if s.get("deviceName"):
        sec["device_name"] = s["deviceName"]
    if s.get("userAgent"):
        sec["user_agent"] = s["userAgent"]

    with open(RCLONE_CONF, "w") as f:
        cp.write(f)
    print("session applied to remote %s (oauth_bundle is now set)" % remote)
    return 0


if __name__ == "__main__":
    sys.exit(main())

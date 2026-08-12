#!/usr/bin/env python3
"""O2 reauth: check o2native session; if expired, pop login window and update config."""
import configparser
import hmac
import hashlib
import json
import os
import subprocess
import sys
import time
import urllib.parse
import urllib.request

HOME = os.path.expanduser("~")
RCLONE_CONF = os.path.join(HOME, ".config/rclone/rclone.conf")
GATEWAY_DIR = os.path.join(HOME, "o2cloud-gateway")
GATEWAY_URL = "http://127.0.0.1:8088"
GATEWAY_ENCRYPTION_KEY = os.path.join(GATEWAY_DIR, "secrets/app_encryption_key.txt")
GATEWAY_ADMIN_PASS = os.path.join(GATEWAY_DIR, "secrets/admin_password.txt")
USER_AGENT = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36"


def log(msg):
    print("[o2-reauth]", msg, flush=True)


def current_session(remote):
    cp = configparser.ConfigParser()
    cp.read(RCLONE_CONF)
    if remote not in cp:
        return None
    s = cp[remote]
    return {
        "validation_key": s.get("validation_key", ""),
        "cookie_jsessionid": s.get("cookie_jsessionid", ""),
    }


def check_session(vk, js):
    url = ("https://cloud.o2online.es/sapi/media"
           "?action=get-storage-space&softdeleted=true&validationkey=" + vk)
    cmd = [
        "curl", "-sS", "-m", "15",
        "-H", "Cookie: JSESSIONID=" + js,
        "-H", "X-deviceid: O2CloudRclone",
        "-H", "X-devicename: O2 Cloud",
        "-H", "Accept: application/json",
        "-H", "Origin: https://cloud.o2online.es",
        "-H", "Referer: https://cloud.o2online.es/",
        url,
    ]
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=20)
        body = r.stdout
        return '"data"' in body and '"error"' not in body
    except Exception:
        return False


def gateway_admin():
    with open(GATEWAY_ADMIN_PASS) as f:
        password = f.read().strip()
    data = urllib.parse.urlencode({"username": "admin", "password": password}).encode()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor())
    opener.open(
        urllib.request.Request(GATEWAY_URL + "/admin/login", data=data, method="POST"),
        timeout=10,
    )
    cookie = ""
    for handler in opener.handlers:
        if hasattr(handler, "cookiejar"):
            for c in handler.cookiejar:
                if c.name == "admin_session":
                    cookie = c.value
    with open(GATEWAY_ENCRYPTION_KEY) as f:
        secret = f.read().rstrip("\n").encode()
    csrf = hmac.new(secret, ("csrf:" + cookie).encode(), hashlib.sha256).hexdigest()
    return opener, csrf


def trigger_login(opener, csrf):
    req = urllib.request.Request(
        GATEWAY_URL + "/api/admin/o2/login",
        method="POST",
        headers={"X-CSRF-Token": csrf},
    )
    return json.loads(opener.open(req, timeout=10).read().decode())


def wait_login(opener, timeout=600):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            req = urllib.request.Request(GATEWAY_URL + "/api/admin/o2/login/status")
            st = json.loads(opener.open(req, timeout=10).read().decode())
            if st.get("state") == "succeeded":
                return True
            if st.get("state") == "failed":
                log("login failed: " + str(st.get("error")))
                return False
        except Exception:
            pass
        time.sleep(5)
    log("login timed out")
    return False


def read_gateway_session():
    reader = os.path.join(os.path.dirname(os.path.abspath(__file__)), "o2_read_gateway_session.py")
    r = subprocess.run(
        [os.path.join(GATEWAY_DIR, ".venv/bin/python3"), reader],
        capture_output=True, text=True, timeout=30,
    )
    lines = r.stdout.strip().splitlines()
    if r.returncode != 0 or len(lines) < 2 or lines[0] == "NONE":
        return None
    return {"validation_key": lines[0], "cookie_jsessionid": lines[1]}


def update_config(remote, sess):
    cp = configparser.ConfigParser()
    cp.read(RCLONE_CONF)
    if remote not in cp:
        log("remote %s not found" % remote)
        return False
    cp[remote]["validation_key"] = sess["validation_key"]
    if sess.get("cookie_jsessionid"):
        cp[remote]["cookie_jsessionid"] = sess["cookie_jsessionid"]
    with open(RCLONE_CONF, "w") as f:
        cp.write(f)
    return True


def main():
    force = "--force" in sys.argv
    remote = "o2native"
    if "--remote" in sys.argv:
        remote = sys.argv[sys.argv.index("--remote") + 1]

    session = current_session(remote)
    if not session:
        log("remote %s not found" % remote)
        return 2

    if not force and session["validation_key"] and check_session(
        session["validation_key"], session.get("cookie_jsessionid", "")
    ):
        log("session OK for %s" % remote)
        return 0

    log("session expired for %s, opening login popup..." % remote)
    try:
        opener, csrf = gateway_admin()
        trigger_login(opener, csrf)
        log("login window opened - complete O2 login in the Chromium window")
    except Exception as e:
        log("could not trigger gateway login: %s" % e)
        return 2

    if not wait_login(opener):
        return 1
    new_session = read_gateway_session()
    if not new_session:
        log("could not read fresh session")
        return 1
    if update_config(remote, new_session):
        log("session updated for %s" % remote)
        return 0
    return 1


if __name__ == "__main__":
    sys.exit(main())

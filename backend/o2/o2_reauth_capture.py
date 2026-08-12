"""mitmproxy script: captures O2 Cloud session from the running app.

Usage:
    mitmdump -s o2_reauth_capture.py --set ssl_insecure=true -p 8080

Then launch the O2 Cloud app with the proxy:
    https_proxy=http://127.0.0.1:8080 O2 Cloud

Or set system proxy to 127.0.0.1:8080 and restart the app.
"""
import json
from mitmproxy import http, ctx

captured = {}

def response(flow: http.HTTPFlow):
    auth = flow.request.headers.get("authorization", "")
    url = flow.request.url
    if "oauth" in auth and "sapi" in url:
        from urllib.parse import urlparse, parse_qs
        params = parse_qs(urlparse(url).query)
        bundle = auth.replace("oauth ", "")
        vk = params.get("validationkey", [""])[0]
        jsid = ""
        cookie = flow.request.headers.get("cookie", "")
        if "JSESSIONID=" in cookie:
            jsid = cookie.split("JSESSIONID=")[1].split(";")[0]
        device_id = flow.request.headers.get("x-deviceid", "")
        device_name = flow.request.headers.get("x-devicename", "")

        if bundle and vk:
            captured.update({
                "validationKey": vk,
                "oauthBundle": bundle,
                "jsessionid": jsid,
                "deviceId": device_id,
                "deviceName": device_name or "O2 Cloud",
                "userAgent": "O2CloudRclone/0.1",
            })
            with open("o2_session.json", "w") as f:
                json.dump(captured, f, indent=2)
            ctx.log.info("SESSION CAPTURED: vk=%s bundle_len=%d jsid=%s" % (vk, len(bundle), jsid[:20]))

def done():
    if captured:
        with open("o2_session.json", "w") as f:
            json.dump(captured, f, indent=2)
        ctx.log.info("Session saved to o2_session.json")
    else:
        ctx.log.warn("No session captured")

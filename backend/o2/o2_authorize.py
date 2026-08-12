#!/usr/bin/env python3
"""
O2 Cloud authorize helper for rclone o2 backend.

Opens a Chromium browser for O2 Cloud login, captures the session,
and outputs JSON that can be pasted into rclone config.

Usage:
    python3 o2_authorize.py

Requires: playwright (pip install playwright && playwright install chromium)
"""

import asyncio
import json
import re
import sys
import os

try:
    from playwright.async_api import async_playwright
except ImportError:
    print("Error: playwright not installed. Run:", file=sys.stderr)
    print("  pip install playwright && playwright install chromium", file=sys.stderr)
    sys.exit(1)


O2_LOGIN_URL = "https://cloud.o2online.es/"
O2_API_BASE  = "https://cloud.o2online.es/sapi/"

VALIDATION_RE = re.compile(r"validationkey[^A-Za-z0-9._-]+([A-Za-z0-9._-]{8,})", re.I)


async def capture_session(timeout_seconds=300):
    seen_urls = []
    oauth_state = {"bundle": "", "device_id": "", "device_name": ""}

    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=False)
        context = await browser.new_context()
        page = context.pages[0] if context.pages else await context.new_page()

        # intercept oauth login requests/responses
        async def on_request(request):
            if "/login/oauth" in request.url:
                headers = await request.all_headers()
                auth = headers.get("authorization", "")
                if auth.startswith("oauth"):
                    oauth_state["bundle"] = auth.split(" ", 1)[1].strip()
                oauth_state["device_id"] = headers.get("x-deviceid", "")
                oauth_state["device_name"] = headers.get("x-devicename", "")

        async def on_response(response):
            if "/login/oauth" in response.url:
                try:
                    headers = response.headers
                    auth = headers.get("authorization", "")
                    if auth.startswith("oauth"):
                        oauth_state["bundle"] = auth.split(" ", 1)[1].strip()
                except Exception:
                    pass
                try:
                    data = await response.json()
                    if isinstance(data, dict) and "data" in data:
                        d = data["data"]
                        if isinstance(d, dict):
                            if d.get("access_token"):
                                oauth_state["bundle"] = d["access_token"]
                            if d.get("accessToken"):
                                oauth_state["bundle"] = d["accessToken"]
                except Exception:
                    pass

        page.on("request", on_request)
        page.on("response", on_response)

        await page.goto(O2_LOGIN_URL)
        print("Please complete the O2 Cloud login in the browser window.", file=sys.stderr)

        deadline = asyncio.get_event_loop().time() + timeout_seconds
        while asyncio.get_event_loop().time() < deadline:
            await asyncio.sleep(2)

            # check for validation key in page
            try:
                state = await page.evaluate("""() => {
                    const out = {};
                    try {
                        for (let i = 0; i < localStorage.length; i++) {
                            const key = localStorage.key(i);
                            out[key] = localStorage.getItem(key);
                        }
                    } catch (_) {}
                    try {
                        for (const entry of performance.getEntriesByType('resource')) {
                            if (entry && entry.name) out['__url_' + entry.name] = entry.name;
                        }
                    } catch (_) {}
                    return JSON.stringify(out);
                }""")
            except Exception:
                state = "{}"

            match = VALIDATION_RE.search(state)
            validation_key = match.group(1) if match else ""

            # also try to find validation key in seen URLs
            if not validation_key:
                text = json.dumps(seen_urls)
                match = VALIDATION_RE.search(text)
                validation_key = match.group(1) if match else ""

            if validation_key and oauth_state.get("bundle"):
                # get cookies
                cookies = await context.cookies(O2_API_BASE)
                jsessionid = ""
                for cookie in cookies:
                    if cookie.get("name") == "JSESSIONID":
                        jsessionid = cookie.get("value", "")
                        break

                ua = ""
                try:
                    ua = await page.evaluate("() => navigator.userAgent || ''")
                except Exception:
                    pass

                session = {
                    "validationKey": validation_key,
                    "oauthBundle": oauth_state.get("bundle", ""),
                    "jsessionid": jsessionid,
                    "deviceId": oauth_state.get("device_id", "O2CloudRclone"),
                    "deviceName": oauth_state.get("device_name", "O2Cloud"),
                    "userAgent": ua or "O2CloudRclone/0.1",
                }
                await browser.close()
                return session

            # track URLs
            try:
                for entry in await page.evaluate("""() => {
                    return Array.from(performance.getEntriesByType('resource'))
                        .filter(e => e.name && e.name.includes('/sapi/'))
                        .map(e => e.name);
                }"""):
                    if entry not in seen_urls:
                        seen_urls.append(entry)
                        if len(seen_urls) > 200:
                            seen_urls.pop(0)
            except Exception:
                pass

        await browser.close()
        return None


def main():
    try:
        session = asyncio.run(capture_session(timeout_seconds=int(sys.argv[1]) if len(sys.argv) > 1 else 300))
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)

    if session is None:
        print("Login timed out or failed.", file=sys.stderr)
        sys.exit(1)

    json.dump(session, sys.stdout, indent=2)
    print(file=sys.stdout)


if __name__ == "__main__":
    main()

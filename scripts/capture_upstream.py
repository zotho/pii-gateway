"""mitmproxy addon: dump every request LiteLLM sends upstream to .run/real-upstream/NNNNN.json.

Auth-ish header values are redacted (only the scheme/prefix is kept). Body is JSON-decoded when possible.
"""
import itertools
import json
import os
import time
from pathlib import Path

OUT = Path(os.environ.get("CAPTURE_DIR", Path(__file__).resolve().parents[1] / ".run" / "real-upstream"))
ID_MARKERS = ("request-id", "request_id", "cf-ray", "trace", "x-oai")
SENSITIVE = {"authorization", "x-api-key", "cookie", "chatgpt-account-id", "proxy-authorization", "x-litellm-api-key"}


class Capture:
    def __init__(self):
        OUT.mkdir(parents=True, exist_ok=True)
        existing = [int(p.stem) for p in OUT.glob("*.json") if p.stem.isdigit()]
        self.n = itertools.count(max(existing, default=0) + 1)

    def request(self, flow):
        r = flow.request
        hdrs = {}
        for k, v in r.headers.items():
            if k.lower() in SENSITIVE:
                first = v.split(" ")[0]
                hdrs[k] = (first + " <redacted>") if first.lower() in ("bearer", "basic") else "<redacted>"
            else:
                hdrs[k] = v
        try:
            raw = r.get_content(strict=False) or b""
        except Exception:
            raw = r.raw_content or b""
        text = raw.decode("utf-8", "replace")
        try:
            body = json.loads(text) if text.strip() else None
        except ValueError:
            body = {"_raw": text[:20000]}
        rec = {"ts": time.time(), "method": r.method, "url": r.pretty_url, "headers": hdrs, "body": body}
        i = next(self.n)
        flow.metadata["cap_id"] = i
        (OUT / f"{i:05d}.json").write_text(json.dumps(rec, ensure_ascii=False, indent=1))

    def response(self, flow):
        i = flow.metadata.get("cap_id")
        if i is None:
            return
        p = OUT / f"{i:05d}.json"
        try:
            rec = json.loads(p.read_text())
            rec["status"] = flow.response.status_code
            # response header NAMES only, plus the values of request-id-like ones (never cookies / tokens)
            rec["response_header_names"] = sorted({k.lower() for k in flow.response.headers.keys()})
            rec["response_request_ids"] = {k: v for k, v in flow.response.headers.items()
                                           if any(m in k.lower() for m in ID_MARKERS)}
            if flow.response.status_code >= 400:
                rec["error_body"] = (flow.response.get_text(strict=False) or "")[:2000]
            p.write_text(json.dumps(rec, ensure_ascii=False, indent=1))
        except Exception:
            pass


addons = [Capture()]

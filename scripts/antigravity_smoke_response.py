"""Sanitized smoke diagnostics. Never return server messages or unlisted values."""
import json
import re

ALLOWED_CODES = frozenset("unsupported_target unsupported_feature unsupported_capability unsupported_protocol_or_mode invalid_request invalid_api_key authentication_unavailable accounting_unavailable permission_denied rate_limit_exceeded rate_limited request_budget_exceeded upstream_error upstream_unavailable connector_unavailable unauthenticated invalid_response request_timeout request_cancelled".split())
DIAGNOSTIC_BRANCHES = frozenset({"http_error", "transport_error", "invalid_response"})
UPSTREAM_STATUSES = frozenset({"UNAUTHENTICATED", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "INVALID_ARGUMENT", "other", "none"})
TRANSPORT_CLASSES = frozenset({"timeout", "connection_refused", "dns", "eof", "tls", "other", "none"})
STREAM_FAILURES = frozenset({
    ("precommit", "precommit_framing"),
    ("precommit", "precommit_eof"),
    ("precommit", "precommit_invalid_json"),
    ("precommit", "precommit_unsupported_finish"),
    ("precommit", "precommit_unsupported_reasoning"),
    ("precommit", "precommit_invalid_usage"),
    ("precommit", "precommit_invalid_function"),
    ("precommit", "precommit_empty_content"),
    ("precommit", "precommit_bounds_exceeded"),
    ("precommit", "precommit_invalid_content"),
    ("precommit", "precommit_invalid_order"),
    ("postcommit", "postcommit_error"),
})


def stream_diagnostics(log_text):
    """Extract only the pairs emitted by stream.go logStreamError."""
    pattern = re.compile(r'msg="Antigravity stream failure" phase="?([a-z_]+)"? category="?([a-z_]+)"?(?=\s|$)')
    found = []
    for line in log_text.splitlines():
        match = pattern.search(line)
        if match and match.groups() in STREAM_FAILURES:
            phase, category = match.groups()
            found.append(f"phase={phase} category={category}")
    return found


def inference_diagnostics(log_text):
    """Return only fixed-schema Antigravity inference diagnostic fields."""
    found = []
    pattern = re.compile(r'msg="Antigravity inference diagnostic" status=(\d+) branch="?([a-z_]+)"? upstream_status="?([A-Z_]+|other|none)"? transport_error_class="?([a-z_]+)"?')
    for line in log_text.splitlines():
        match = pattern.search(line)
        if not match:
            continue
        status, branch, upstream, transport = match.groups()
        if int(status) > 599 or branch not in DIAGNOSTIC_BRANCHES or upstream not in UPSTREAM_STATUSES or transport not in TRANSPORT_CLASSES:
            continue
        found.append(f"status={status} branch={branch} upstream_status={upstream} transport_error_class={transport}")
    return found


def diagnostic(body, content_type):
    media = content_type.split(";", 1)[0].strip().lower()
    safe_media = media if media in {"application/json", "text/event-stream", "text/plain", "text/html"} else "other"
    objects = []
    try:
        if media == "text/event-stream":
            for block in body.decode("utf-8").replace("\r\n", "\n").split("\n\n"):
                data = "\n".join(line[5:].lstrip() for line in block.splitlines() if line.startswith("data:"))
                if data and data != "[DONE]":
                    objects.append(json.loads(data))
        else:
            objects.append(json.loads(body))
    except (ValueError, UnicodeError):
        return f"fixed_code=none reason=non_json_or_malformed content_type={safe_media} bytes={len(body)}"
    # Traverse structure, not strings: never interpret a message as a code.
    def codes(value):
        if isinstance(value, dict):
            code = value.get("code")
            if isinstance(code, str) and code in ALLOWED_CODES:
                yield code
            for name, child in value.items():
                if name != "message":
                    yield from codes(child)
        elif isinstance(value, list):
            for child in value:
                yield from codes(child)
    code = next((code for obj in objects for code in codes(obj)), None)
    return f"fixed_code={code or 'none'} reason={'recognized_code' if code else 'missing_or_unlisted_code'} content_type={safe_media} bytes={len(body)}"

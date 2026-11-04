#!/usr/bin/env python3
"""Two-turn Responses tool runner; emits only fixed diagnostics and aggregates."""
import http.client
import json
import sys

from antigravity_smoke_response import ALLOWED_CODES, diagnostic

MODEL = "gemini-2.5-flash"
TOOL = {"type": "function", "name": "add", "description": "Add two bounded integers.",
        "parameters": {"type": "object", "properties": {
            "a": {"type": "integer"}, "b": {"type": "integer"}},
            "required": ["a", "b"], "additionalProperties": False}}
INCLUDE = ["reasoning.encrypted_content"]
PROMPT = "Add 17 and 25."
MAX_BYTES = 8 * 1024 * 1024


class InvalidStream(Exception):
    pass


def _safe_code(body, content_type):
    summary = diagnostic(body, content_type)
    code = summary.partition("fixed_code=")[2].split(" ", 1)[0]
    return code if code in ALLOWED_CODES else "none"


def _new_turn():
    return {"status": "none", "failure": "transport_error", "fixed_code": "none",
            "sse_events": 0, "text_deltas": 0, "function_call_events": 0,
            "function_call_items": 0, "terminal": False, "completed": False,
            "nonempty": False, "carrier_count": 0, "carrier_type": "absent",
            "carrier_string_count": 0, "carrier_null_count": 0,
            "carrier_other_count": 0, "output": None,
            "tool_name_matched": "not_checked", "args_valid": "not_checked"}


def _observe_carrier(turn, item):
    if not isinstance(item, dict) or item.get("type") != "reasoning" or "encrypted_content" not in item:
        return
    value = item["encrypted_content"]
    kind = "string" if isinstance(value, str) else "null" if value is None else "other"
    turn[f"carrier_{kind}_count"] += 1
    turn["carrier_count"] += 1
    present = [name for name in ("string", "null", "other") if turn[f"carrier_{name}_count"]]
    turn["carrier_type"] = present[0] if len(present) == 1 else "mixed"


def _carrier_metrics(turn, items):
    turn["carrier_count"] = 0
    turn["carrier_type"] = "absent"
    turn["carrier_string_count"] = 0
    turn["carrier_null_count"] = 0
    turn["carrier_other_count"] = 0
    if isinstance(items, list):
        for item in items:
            _observe_carrier(turn, item)


def _event(turn, name, payload):
    try:
        data = json.loads(payload)
    except (ValueError, TypeError):
        raise InvalidStream() from None
    if not isinstance(data, dict):
        raise InvalidStream()
    turn["sse_events"] += 1
    if name == "response.output_text.delta":
        turn["text_deltas"] += 1
        delta = data.get("delta")
        turn["nonempty"] |= isinstance(delta, str) and bool(delta.strip())
    elif name.startswith("response.function_call_arguments."):
        turn["function_call_events"] += 1
    elif name in ("response.output_item.added", "response.output_item.done"):
        item = data.get("item")
        if isinstance(item, dict) and item.get("type") == "function_call":
            turn["function_call_items"] += name == "response.output_item.added"
        if name == "response.output_item.done" and isinstance(item, dict) and item.get("type") == "reasoning":
            _observe_carrier(turn, item)
    elif name == "response.failed" or name == "response.incomplete":
        turn["terminal"] = True
        turn["failure"] = "stream_failed"
        frame = f"event: {name}\ndata: {payload}\n\n".encode()
        turn["fixed_code"] = _safe_code(frame, "text/event-stream")
        return True
    elif name == "response.completed":
        turn["terminal"] = True
        response = data.get("response")
        if not isinstance(response, dict) or response.get("status") != "completed" or not isinstance(response.get("output"), list):
            raise InvalidStream()
        turn["completed"] = True
        turn["failure"] = "none"
        turn["output"] = response["output"]
        turn["function_call_items"] = sum(
            isinstance(item, dict) and item.get("type") == "function_call" for item in response["output"])
        _carrier_metrics(turn, response["output"])
        return True
    return False


def _response(connection_factory, key, request):
    turn = _new_turn()
    conn = None
    response = None
    try:
        conn = connection_factory()
        conn.key = key
        conn.request("POST", "/v1/responses", json.dumps(request, separators=(",", ":")), {
            "Authorization": "Bearer " + conn.key, "Content-Type": "application/json"})
        response = conn.getresponse()
        turn["status"] = response.status
        if not 200 <= response.status < 300:
            body = response.read(65536)
            turn["failure"] = "http_error"
            turn["fixed_code"] = _safe_code(body, response.getheader("Content-Type", ""))
            return turn

        event_name = None
        data_lines = []
        total_bytes = 0
        for raw in response:
            total_bytes += len(raw)
            if total_bytes > MAX_BYTES or len(raw) > 2 * 1024 * 1024:
                raise InvalidStream()
            line = raw.decode("utf-8", "strict").rstrip("\r\n")
            if not line:
                if event_name is not None or data_lines:
                    if _event(turn, event_name or "message", "\n".join(data_lines)):
                        break
                event_name, data_lines = None, []
            elif line.startswith(":"):
                continue
            elif line.startswith("event:"):
                if event_name is not None:
                    raise InvalidStream()
                event_name = line[6:].strip()
            elif line.startswith("data:"):
                data_lines.append(line[5:].lstrip())
        else:
            if event_name is not None or data_lines:
                raise InvalidStream()
        if turn["failure"] == "transport_error":
            turn["failure"] = "no_completed"
    except InvalidStream:
        turn["failure"] = "invalid_stream"
    except Exception:
        turn["failure"] = "transport_error"
    finally:
        if response is not None:
            try:
                response.close()
            except Exception:
                pass
        if conn is not None:
            try:
                conn.close()
            except Exception:
                pass
    return turn


def _tool_call(turn):
    output = turn["output"]
    if not isinstance(output, list):
        turn["failure"] = "invalid_stream"
        return None
    calls = [item for item in output if isinstance(item, dict) and item.get("type") == "function_call"]
    unexpected_calls = [item for item in output if isinstance(item, dict)
                        and isinstance(item.get("type"), str)
                        and item["type"].endswith("_call") and item["type"] != "function_call"]
    if len(calls) != 1 or unexpected_calls:
        turn["failure"] = "invalid_tool"
        turn["tool_name_matched"] = False
        turn["args_valid"] = False
        return None
    call = calls[0]
    if call.get("name") != "add":
        turn["failure"] = "invalid_tool"
        turn["tool_name_matched"] = False
        turn["args_valid"] = False
        return None
    turn["tool_name_matched"] = True
    raw_args = call.get("arguments")
    try:
        args = json.loads(raw_args) if isinstance(raw_args, str) else None
    except (ValueError, TypeError):
        args = None
    if (not isinstance(args, dict) or set(args) != {"a", "b"}
            or any(type(args[key]) is not int or not -1000 <= args[key] <= 1000 for key in ("a", "b"))):
        turn["failure"] = "invalid_arguments"
        turn["args_valid"] = False
        return None
    turn["args_valid"] = True
    call_id = call.get("call_id")
    if not isinstance(call_id, str) or not call_id:
        turn["failure"] = "invalid_tool"
        return None
    return call_id, args["a"] + args["b"]


def _emit(turn_number, turn):
    status = turn["status"] if isinstance(turn["status"], int) and 100 <= turn["status"] <= 599 else "none"
    print(" ".join((
        f"turn={turn_number}", f"status={status}", f"failure={turn['failure']}",
        f"fixed_code={turn['fixed_code']}", f"sse_events={turn['sse_events']}",
        f"text_deltas={turn['text_deltas']}", f"function_call_events={turn['function_call_events']}",
        f"function_call_items={turn['function_call_items']}",
        f"tool_name_matched={str(turn['tool_name_matched']).lower()}",
        f"args_valid={str(turn['args_valid']).lower()}",
        f"terminal={str(turn['terminal']).lower()}", f"completed={str(turn['completed']).lower()}",
        f"nonempty={str(turn['nonempty']).lower()}",
        f"carrier_present={str(turn['carrier_count'] > 0).lower()}",
        f"carrier_count={turn['carrier_count']}", f"carrier_type={turn['carrier_type']}",
        f"carrier_string_count={turn['carrier_string_count']}",
        f"carrier_null_count={turn['carrier_null_count']}",
        f"carrier_other_count={turn['carrier_other_count']}")))


def run(connection_factory, key):
    first = _response(connection_factory, key, {
        "model": MODEL, "stream": True, "input": PROMPT,
        "include": INCLUDE, "tools": [TOOL],
        "tool_choice": {"type": "function", "name": "add"}})
    tool = _tool_call(first) if first["failure"] == "none" else None
    _emit(1, first)
    if tool is None:
        print("runner=failed dispatches_bounded=true")
        return 1

    call_id, total = tool
    first["tool_call_output"] = {"type": "function_call_output", "call_id": call_id, "output": str(total)}
    second_input = [{"type": "message", "role": "user", "content": [
        {"type": "input_text", "text": PROMPT}]}] + first["output"] + [first["tool_call_output"]]
    second = _response(connection_factory, key, {
        "model": MODEL, "stream": True, "input": second_input,
        "include": INCLUDE, "tools": [TOOL]})
    if second["failure"] == "none":
        if any(isinstance(item, dict) and isinstance(item.get("type"), str)
               and item["type"].endswith("_call") for item in second["output"]):
            second["failure"] = "invalid_tool"
            second["tool_name_matched"] = False
            second["args_valid"] = False
        else:
            second["tool_name_matched"] = "not_applicable"
            second["args_valid"] = "not_applicable"
            if not second["nonempty"]:
                second["failure"] = "nonempty_required"
            elif not any(isinstance(item, dict) and item.get("type") == "message" for item in second["output"]):
                second["failure"] = "invalid_stream"
    else:
        second["tool_name_matched"] = "not_applicable"
        second["args_valid"] = "not_applicable"
    _emit(2, second)
    if second["failure"] != "none":
        print("runner=failed dispatches_bounded=true")
        return 1
    print("dispatches=2 replay_accepted=true")
    return 0


def main():
    try:
        key = sys.stdin.readline().rstrip("\r\n")
        if not key or sys.stdin.readline():
            print("runner=failed stage=transport_error")
            return 1
        port = int(sys.argv[1])
        return run(lambda: http.client.HTTPConnection("127.0.0.1", port, timeout=90), key)
    except Exception:
        # Exception text may include status, headers, or private bytes.
        print("runner=failed stage=transport_error")
        return 1
if __name__ == "__main__":
    raise SystemExit(main())

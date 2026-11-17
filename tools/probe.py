"""Probe: records what the gateway hands a route, one JSON line per run, in
tools/probe-seen.ndjson. Use it to see what a new bot or event actually
sends before writing the real handler. It acts on nothing."""
import json, os, sys, datetime
ev = json.load(sys.stdin) if os.environ.get("GATEWAY_REASON") == "event" else {}
msg = ev.get("event", {}).get("message", {})
line = {
    "at": datetime.datetime.now().isoformat(timespec="seconds"),
    "reason": os.environ.get("GATEWAY_REASON"),
    "event_id": os.environ.get("GATEWAY_EVENT_ID"),
    "chat_id": msg.get("chat_id"),
    "chat_type": msg.get("chat_type"),
    "message_type": msg.get("message_type"),
    "content": msg.get("content"),
    "sender_type": ev.get("event", {}).get("sender", {}).get("sender_type"),
}
with open(os.path.join(os.path.dirname(__file__), "probe-seen.ndjson"), "a", encoding="utf-8") as f:
    f.write(json.dumps(line, ensure_ascii=False) + "\n")
print("probe:", line["reason"], line["message_type"], line["chat_id"])

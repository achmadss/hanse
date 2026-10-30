#!/usr/bin/env python3
"""echo capability handler, hanse.handler/v1.

Read the whole request from stdin, write one response to stdout, and use
stderr for log lines only.
"""
import json
import os
import signal
import sys
import time

CONTRACT = "hanse.handler/v1"
STOP = False


def on_term(_signum, _frame):
    global STOP
    STOP = True


def log(line):
    print(line, file=sys.stderr, flush=True)


def reply(status, **fields):
    body = {"contract": CONTRACT, "status": status}
    body.update(fields)
    json.dump(body, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")
    sys.stdout.flush()
    return 0 if status != "error" else 1


def error(code, message):
    return reply("error", error={"code": code, "message": message})


def stop_requested():
    cancel_file = os.environ.get("HANSE_CANCEL_FILE")
    return STOP or (cancel_file is not None and os.path.exists(cancel_file))


def options(req):
    if req.get("field") != "seconds":
        return error("unknown_field", f"no options for {req.get('field')}")
    return reply("ok", result={"options": [
        {"value": 5, "label": "5 seconds"},
        {"value": 60, "label": "1 minute"},
    ]})


def say(req):
    message = req.get("arguments", {}).get("message")
    if not isinstance(message, str) or message == "":
        return error("missing_argument", "the message argument is required")
    log(f"say: {message}")
    return reply("ok", result={"said": message})


def sleep(req):
    seconds = req.get("arguments", {}).get("seconds")
    if not isinstance(seconds, int) or seconds < 1:
        return error("invalid_arguments", "the seconds argument must be an integer")
    for i in range(1, seconds + 1):
        if stop_requested():
            log(f"stop requested, stopping at {i - 1} of {seconds}")
            return reply("cancelled", result={"slept": i - 1})
        log(f"sleep {i} of {seconds}")
        time.sleep(1)
    return reply("ok", result={"slept": seconds})


def main():
    signal.signal(signal.SIGTERM, on_term)
    req = json.load(sys.stdin)
    if req.get("kind") == "options":
        return options(req)
    handlers = {"say": say, "sleep": sleep}
    handler = handlers.get(req.get("operation"))
    if handler is None:
        return error("unknown_operation", f"unknown operation: {req.get('operation')}")
    return handler(req)


if __name__ == "__main__":
    sys.exit(main())

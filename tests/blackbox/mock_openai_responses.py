#!/usr/bin/env python3
"""Deterministic Responses tool loop used by the Codex CLI black-box test."""

from __future__ import annotations

import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


PORT = int(os.getenv("MOCK_OPENAI_PORT", "18082"))
COMMANDS = (
    "echo turn1 > codex-state.txt",
    "echo turn2 >> codex-state.txt",
    "wc -l codex-state.txt && cat codex-state.txt",
)
counter = 0
counter_lock = threading.Lock()


def response_object(response_id: str, status: str, output: list[dict], usage: dict | None = None) -> dict:
    return {
        "id": response_id,
        "object": "response",
        "created_at": 1_787_677_000,
        "status": status,
        "error": None,
        "incomplete_details": None,
        "instructions": None,
        "max_output_tokens": None,
        "model": "codex-mock",
        "output": output,
        "parallel_tool_calls": True,
        "previous_response_id": None,
        "reasoning": {"effort": None, "summary": None},
        "store": False,
        "temperature": None,
        "text": {"format": {"type": "text"}},
        "tool_choice": "auto",
        "tools": [],
        "top_p": None,
        "truncation": "disabled",
        "usage": usage,
        "metadata": {},
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, _format: str, *_args: object) -> None:
        return

    def do_POST(self) -> None:
        global counter
        payload = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        with counter_lock:
            counter += 1
            call = counter
        input_types = [item.get("type") for item in payload.get("input", [])]
        print(f"MOCK_CALL {call} INPUT_TAIL {input_types[-5:]}", flush=True)
        response_id = f"upstream_{call}"
        stage = (call + 1) // 2
        if call % 2:
            item = {
                "id": f"fc_{stage}",
                "type": "function_call",
                "status": "completed",
                "call_id": f"call_{stage}",
                "name": "exec_command",
                "arguments": json.dumps({"cmd": COMMANDS[min(stage - 1, len(COMMANDS) - 1)]}),
            }
            created = response_object(response_id, "in_progress", [])
            completed = response_object(response_id, "completed", [item], usage(10, 5))
            events = (
                ("response.created", {"type": "response.created", "sequence_number": 0, "response": created}),
                ("response.output_item.done", {"type": "response.output_item.done", "sequence_number": 1, "output_index": 0, "item": item}),
                ("response.completed", {"type": "response.completed", "sequence_number": 2, "response": completed}),
            )
        else:
            text = f"TURN_{stage}_DONE"
            item = {
                "id": f"msg_{stage}",
                "type": "message",
                "status": "completed",
                "role": "assistant",
                "content": [{"type": "output_text", "text": text, "annotations": []}],
            }
            created = response_object(response_id, "in_progress", [])
            completed = response_object(response_id, "completed", [item], usage(12, 3))
            events = (
                ("response.created", {"type": "response.created", "sequence_number": 0, "response": created}),
                ("response.output_item.added", {"type": "response.output_item.added", "sequence_number": 1, "output_index": 0, "item": {**item, "status": "in_progress", "content": []}}),
                ("response.content_part.added", {"type": "response.content_part.added", "sequence_number": 2, "item_id": item["id"], "output_index": 0, "content_index": 0, "part": {"type": "output_text", "text": "", "annotations": []}}),
                ("response.output_text.delta", {"type": "response.output_text.delta", "sequence_number": 3, "item_id": item["id"], "output_index": 0, "content_index": 0, "delta": text, "logprobs": []}),
                ("response.content_part.done", {"type": "response.content_part.done", "sequence_number": 4, "item_id": item["id"], "output_index": 0, "content_index": 0, "part": item["content"][0]}),
                ("response.output_item.done", {"type": "response.output_item.done", "sequence_number": 5, "output_index": 0, "item": item}),
                ("response.completed", {"type": "response.completed", "sequence_number": 6, "response": completed}),
            )
        body = "".join(f"event: {name}\ndata: {json.dumps(event)}\n\n" for name, event in events).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def usage(input_tokens: int, output_tokens: int) -> dict:
    return {
        "input_tokens": input_tokens,
        "output_tokens": output_tokens,
        "total_tokens": input_tokens + output_tokens,
        "input_tokens_details": {"cached_tokens": 0},
    }


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()

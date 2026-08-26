#!/usr/bin/env python3
"""Black-box conformance checks using the official OpenAI Python SDK."""

from __future__ import annotations

import os

import openai
from openai import OpenAI


BASE_URL = os.getenv("GATEWAY_BASE_URL", "http://127.0.0.1:8080/v1")
API_KEY = os.getenv("GATEWAY_API_KEY", "dev-token")
MODEL = os.getenv("GATEWAY_MODEL", "echo-v1")


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def main() -> None:
    client = OpenAI(api_key=API_KEY, base_url=BASE_URL, max_retries=0, timeout=15)

    models = client.models.list()
    require(any(model.id == MODEL for model in models.data), f"{MODEL} missing from model catalog")

    first = client.responses.create(model=MODEL, input="SDK_TURN_1")
    require(first.output_text == "SDK_TURN_1", f"unexpected first output: {first.output_text!r}")
    require(client.responses.retrieve(first.id).id == first.id, "response retrieval changed the id")
    input_items = client.responses.input_items.list(first.id)
    require(len(input_items.data) == 1, "response input-items list is incomplete")

    second = client.responses.create(
        model=MODEL,
        previous_response_id=first.id,
        input="SDK_TURN_2",
    )
    require(second.previous_response_id == first.id, "previous_response_id was not preserved")
    require(second.output_text == "SDK_TURN_1SDK_TURN_2", f"multi-turn context was lost: {second.output_text!r}")

    response_events: list[str] = []
    response_deltas: list[str] = []
    for event in client.responses.create(model=MODEL, input="SDK_RESPONSE_STREAM", stream=True):
        response_events.append(event.type)
        if event.type == "response.output_text.delta":
            response_deltas.append(event.delta)
    require(response_events[0] == "response.created", "Responses stream did not start with response.created")
    require(response_events[-1] == "response.completed", "Responses stream did not finish with response.completed")
    require("".join(response_deltas) == "SDK_RESPONSE_STREAM", "Responses stream text did not round-trip")

    chat = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "SDK_CHAT"}],
    )
    require(chat.choices[0].message.content == "SDK_CHAT", "Chat Completions text did not round-trip")

    chat_chunks = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "SDK_CHAT_STREAM"}],
        stream=True,
    )
    chat_text = "".join(
        chunk.choices[0].delta.content or ""
        for chunk in chat_chunks
        if chunk.choices
    )
    require(chat_text == "SDK_CHAT_STREAM", "Chat Completions stream text did not round-trip")

    conversation = client.conversations.create(metadata={"suite": "openai-sdk-blackbox"})
    conversation_response = client.responses.create(
        model=MODEL,
        conversation=conversation.id,
        input="SDK_CONVERSATION",
    )
    require(conversation_response.output_text == "SDK_CONVERSATION", "Conversation response text did not round-trip")
    conversation_items = client.conversations.items.list(conversation.id)
    require(len(conversation_items.data) == 2, "Conversation did not retain user and assistant items")
    deleted_conversation = client.conversations.delete(conversation.id)
    require(deleted_conversation.deleted, "Conversation deletion was not confirmed")

    client.responses.delete(first.id)
    client.responses.delete(second.id)

    print(f"PASS openai-sdk={openai.__version__} base_url={BASE_URL} model={MODEL}")


if __name__ == "__main__":
    main()

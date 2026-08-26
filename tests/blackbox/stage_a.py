#!/usr/bin/env python3
import json
import os
import urllib.request
import urllib.error


BASE_URL = os.environ.get("GATEWAY_BASE_URL", "http://127.0.0.1:8080").rstrip("/")
API_KEY = os.environ.get("GATEWAY_API_KEY", "dev-token")
OTHER_API_KEY = os.environ.get("GATEWAY_OTHER_API_KEY", "other-token")
MODEL = os.environ.get("GATEWAY_MODEL", "echo-v1")


def request(path: str, payload=None, api_key=API_KEY):
    body = None if payload is None else json.dumps(payload).encode()
    method = "GET" if payload is None else "POST"
    req = urllib.request.Request(
        BASE_URL + path,
        data=body,
        method=method,
        headers={"Authorization": "Bearer " + api_key, "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=10) as response:
        assert response.status == 200, (path, response.status)
        return json.load(response)


def assert_rejected(path: str, payload):
    try:
        request(path, payload, OTHER_API_KEY)
    except urllib.error.HTTPError as error:
        assert error.code == 400, (path, error.code, error.read())
        return
    raise AssertionError((path, "cross-Tenant route unexpectedly succeeded"))


catalog = request("/v1/capabilities")
profile = next(item for item in catalog["data"] if item["id"] == MODEL)
assert profile["capabilities"] == {
    "embeddings": "native",
    "moderation": "native",
    "rerank": "native",
}

embeddings = request(
    "/v1/embeddings",
    {"model": MODEL, "input": ["first", "second"], "dimensions": 4},
)
assert embeddings["object"] == "list"
assert len(embeddings["data"]) == 2
assert all(len(item["embedding"]) == 4 for item in embeddings["data"])

moderation = request(
    "/v1/moderations",
    {"model": MODEL, "input": ["ordinary text", "unsafe request"]},
)
assert [item["flagged"] for item in moderation["results"]] == [False, True]

rerank = request(
    "/v1/rerank",
    {
        "model": MODEL,
        "query": "red apple",
        "documents": ["blue ocean", "red apple tree", "red bicycle"],
        "top_n": 2,
        "return_documents": True,
    },
)
assert [item["index"] for item in rerank["results"]] == [1, 2]

other_catalog = request("/v1/capabilities", api_key=OTHER_API_KEY)
assert all(item["id"] != MODEL for item in other_catalog["data"])
assert_rejected("/v1/embeddings", {"model": MODEL, "input": "hidden"})
assert_rejected("/v1/moderations", {"model": MODEL, "input": "hidden"})
assert_rejected("/v1/rerank", {"model": MODEL, "query": "hidden", "documents": ["hidden"]})

print("Stage A black box passed: capabilities, embeddings, moderations, rerank, Tenant isolation")

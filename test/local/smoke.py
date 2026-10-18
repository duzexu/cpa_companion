#!/usr/bin/env python3
"""Exercise local OAuth, plans, priority fallback, quota reset, and access gates."""

import http.cookiejar
import json
import os
import re
import secrets
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get("SMOKE_BASE_URL", "http://localhost:18080")
RUN = secrets.token_hex(3)
client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))


def request(path, data=None, headers=None):
    body = urllib.parse.urlencode(data, doseq=True).encode() if isinstance(data, dict) else data
    req = urllib.request.Request(BASE + path, data=body, headers=headers or {})
    with client.open(req, timeout=30) as response:
        return response.read().decode()


def match(pattern, document, label):
    found = re.search(pattern, document)
    if not found:
        raise RuntimeError(f"missing {label}")
    return found.group(1)


def status(path, data=None, headers=None):
    try:
        request(path, data, headers)
    except urllib.error.HTTPError as error:
        return error.code
    return 200


def completion(key, model="local-model"):
    result = request(
        "/v1/chat/completions",
        json.dumps({"model": model, "messages": [{"role": "user", "content": "test"}]}).encode(),
        {"Authorization": "Bearer " + key, "Content-Type": "application/json"},
    )
    return json.loads(result)["choices"][0]["message"]["content"]


def create_plan(csrf, name, accounts=(), reset_first=0, start="", end="", budget=""):
    fields = {
        "csrf": csrf, "name": name, "account_id": list(accounts),
        "reset_local-first.json": reset_first,
        "budget_usd": budget,
    }
    for account in accounts:
        fields["starts_at_" + account] = start
        fields["ends_at_" + account] = end
    request("/admin/plans", fields)
    page = request("/admin")
    return match(r'<strong>' + re.escape(name) + r'</strong><span class="subline mono">([0-9a-f]+)</span>', page, name + " plan ID")


def assign(csrf, plan_id):
    request("/admin/assign", {"csrf": csrf, "user_id": "local-admin", "plan_id": plan_id})


def delete_plan(csrf, plan_id):
    request("/admin/plans/delete", {"csrf": csrf, "id": plan_id})


if request("/healthz") != "ok":
    raise RuntimeError("companion health check failed")
dashboard = request("/auth/login")
if "local-admin" not in dashboard:
    raise RuntimeError("mock OAuth login failed")
admin = request("/admin")
csrf = match(r'name="csrf" value="([^"]+)"', admin, "CSRF token")
if "Local AI provider" not in admin or "mock-upstream-secret" in admin:
    raise RuntimeError("AI provider account missing or upstream key leaked")
if "优先级</th>" not in admin or "local-first" not in admin:
    raise RuntimeError("account priorities missing")

first = "local-first.json"
second = "local-second.json"
main_name = "priority-smoke-" + RUN
plan_id = create_plan(csrf, main_name, (first, second), reset_first=1)
created_plans = [plan_id]
assign(csrf, plan_id)
page = request("/")
if main_name not in page or "已分配账号id" not in page or "<code>local-first</code>" not in page:
    raise RuntimeError("assigned plan missing from dashboard")
if "5 小时限额" not in page or "周限额" not in page or "23%" not in page or "主动重置过期时间" not in page:
    raise RuntimeError("account quota preview missing")
created = request("/keys", {"csrf": csrf, "name": "local-smoke"})
key_id = match(r'<tr><td>local-smoke</td><td><code>([^<]+)</code>', created, "API key ID")
key = json.loads(request("/keys/copy", {"csrf": csrf, "id": key_id}, {"Accept": "application/json"}))["key"]
headers = {"Authorization": "Bearer " + key, "Content-Type": "application/json"}
try:
    if completion(key) != "first-account":
        raise RuntimeError("priority did not choose first account")
    if completion(key, "local-throttle-model") != "second-account":
        raise RuntimeError("rate-limit fallback did not choose second account")
    models = json.loads(request("/v1/models", headers=headers))
    if {item["id"] for item in models["data"]} != {"local-model", "local-throttle-model"}:
        raise RuntimeError("model list did not combine plan accounts")
    if status("/v1/chat/completions", json.dumps({"model": "local-first/local-model"}).encode(), headers) != 404:
        raise RuntimeError("client-supplied account prefix was accepted")
    dashboard = request("/")
    if first in dashboard:
        raise RuntimeError("dashboard exposed upstream account ID")
    reset_ref = match(r'name="account" value="([0-9a-f]+)"', dashboard, "reset account reference")
    if status("/quota/reset", {"csrf": csrf, "account": first}) != 403:
        raise RuntimeError("raw account ID was accepted for reset")
    request("/quota/reset", {"csrf": csrf, "account": reset_ref})
    if "已用 1 / 1" not in request("/"):
        raise RuntimeError("user reset count did not advance")
    if status("/quota/reset", {"csrf": csrf, "account": reset_ref}) != 403:
        raise RuntimeError("reset allowance was not enforced")
    if status("/admin/plans", {"csrf": csrf, "name": "conflicting-smoke-" + RUN, "account_id": [first]}) != 409:
        raise RuntimeError("overlapping account plan was accepted")
    if status("/admin/plans", {"csrf": csrf, "name": "missing-account-smoke-" + RUN}) != 400:
        raise RuntimeError("accountless plan was accepted")
    assign(csrf, "")
    delete_plan(csrf, plan_id)
    created_plans.remove(plan_id)
    price_page = request("/prices")
    if "一键同步价格" not in price_page or "local-model" not in price_page:
        raise RuntimeError("model price page missing")
    request("/admin/prices", {
        "csrf": csrf, "model": "local-model", "expression": 'tier("standard", p * 1 + c * 1)',
    })
    budget_plan = create_plan(csrf, "budget-smoke-" + RUN, (first,), budget="0.0003")
    created_plans.append(budget_plan)
    assign(csrf, budget_plan)
    if completion(key) != "first-account" or completion(key) != "first-account":
        raise RuntimeError("priced requests failed")
    if status("/v1/chat/completions", json.dumps({"model": "local-model"}).encode(), headers) != 403:
        raise RuntimeError("exhausted USD budget allowed traffic")
    if "$0.0003 / $0.0003" not in request("/"):
        raise RuntimeError("budget usage missing from dashboard")
    assign(csrf, "")
    delete_plan(csrf, budget_plan)
    created_plans.remove(budget_plan)
    future = create_plan(csrf, "future-smoke-" + RUN, (first,), start="2099-01-01T00:00")
    created_plans.append(future)
    assign(csrf, future)
    if status("/v1/chat/completions", json.dumps({"model": "local-model"}).encode(), headers) != 403:
        raise RuntimeError("future plan allowed API traffic")
    assign(csrf, "")
    if status("/v1/models", headers=headers) != 403:
        raise RuntimeError("unassigned user allowed API traffic")
finally:
    assign(csrf, "")
    request("/keys/revoke", {"csrf": csrf, "id": key_id})
    for plan in created_plans:
        delete_plan(csrf, plan)

print("PASS: OAuth, exclusive plans, required account, USD budget, quota preview and reset, priority fallback, time and assignment gates, key revocation")

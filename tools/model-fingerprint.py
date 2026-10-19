#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""模型指纹体检：判断上游是否在"以次充好"（换模型 / 缩水实现）。

思路：换模型藏得住"名字"，藏不住这些：
  ① 分词器（tokenizer）—— 同一段文本在不同家族下的 prompt_tokens 差异极大
  ② 上限（max_tokens / context）—— 冒牌货提前失败或悄悄截断
  ③ 能力旗标 —— reasoning 档位、视觉、工具调用、logprobs
  ④ 确定性 —— temperature=0 两次输出应逐字一致
  ⑤ 元数据 —— model 回显 / system_fingerprint / 响应头

用法（两边各跑一次，然后逐项对比）：
  python3 tools/model-fingerprint.py --base http://127.0.0.1:7863/v1 --key sk-xxx --model deepseek-v4.1-flash
  python3 tools/model-fingerprint.py --base <官方 OpenAI 兼容地址>/v1 --key sk-official --model <同名> --json official.json

判据：tokenizer 计数 + 上限 + 能力三项任一不符 = 高度可疑；只有风格/自我描述不同 = 证据不足。
"""
import argparse, base64, hashlib, json, sys, time, urllib.error, urllib.request

PROBES = [
    ("ascii", "The quick brown fox jumps over the lazy dog. " * 4),
    ("cjk", "中华人民共和国人工智能模型指纹体检，用于判断上游是否替换了模型。" * 3),
    ("code", "def f(x):\n    return [i * i for i in range(10)]\n" * 3),
    ("emoji", "\U0001F680\U0001F525\U0001F9EA\U0001F50D\U0001F4CA " * 8),
]

# 1x1 透明 PNG（视觉能力探针；不支持多模态的模型会直接报错）
PNG_1PX = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="
)


def call(base, key, body, timeout):
    url = base.rstrip("/") + "/chat/completions"
    req = urllib.request.Request(
        url, data=json.dumps(body).encode(), method="POST",
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + key},
    )
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read(), time.time() - t0
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read(), time.time() - t0
    except Exception as e:  # 传输层
        return 0, {}, str(e).encode(), time.time() - t0


def parse(raw):
    try:
        return json.loads(raw.decode("utf-8", "replace"))
    except Exception:
        return None


def text_of(obj):
    if not obj:
        return ""
    try:
        return obj["choices"][0]["message"].get("content") or ""
    except Exception:
        return ""


def usage_of(obj):
    if not obj:
        return {}
    u = obj.get("usage") or {}
    return {k: u.get(k) for k in ("prompt_tokens", "completion_tokens", "total_tokens") if k in u}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True, help="OpenAI 兼容 base，如 http://127.0.0.1:7863/v1")
    ap.add_argument("--key", required=True)
    ap.add_argument("--model", required=True)
    ap.add_argument("--timeout", type=float, default=90)
    ap.add_argument("--json", help="把结果写到该文件，便于两边 diff")
    a = ap.parse_args()

    out = {"base": a.base, "model": a.model, "probes": {}, "meta": {}, "caps": {}, "misc": {}}

    # ① 分词器指纹：max_tokens=1，只看 prompt_tokens
    for name, text in PROBES:
        st, hdr, raw, _ = call(a.base, a.key, {
            "model": a.model, "max_tokens": 1,
            "messages": [{"role": "user", "content": text}],
        }, a.timeout)
        obj = parse(raw)
        out["probes"][name] = {"status": st, "usage": usage_of(obj),
                               "chars": len(text), "err": "" if obj else raw[:200].decode("utf-8", "replace")}

    # ② 元数据：model 回显 / system_fingerprint / 响应头
    st, hdr, raw, _ = call(a.base, a.key, {
        "model": a.model, "max_tokens": 8,
        "messages": [{"role": "user", "content": "ping"}],
    }, a.timeout)
    obj = parse(raw) or {}
    out["meta"] = {
        "status": st,
        "model_echo": obj.get("model"),
        "system_fingerprint": obj.get("system_fingerprint"),
        "id_prefix": (obj.get("id") or "")[:12],
        "headers": {k.lower(): v for k, v in hdr.items()
                    if k.lower() in ("server", "x-request-id", "x-served-by", "via", "cf-ray")},
    }

    # ③ 输出上限探针：故意要一个荒谬的 max_tokens，看它报的真实上限
    st, _, raw, _ = call(a.base, a.key, {
        "model": a.model, "max_tokens": 100000000,
        "messages": [{"role": "user", "content": "hi"}],
    }, a.timeout)
    out["caps"]["max_tokens_probe"] = {"status": st, "detail": raw[:400].decode("utf-8", "replace")}

    # ④ reasoning 档位：high 档应产生 reasoning 字段或更长思考
    st, _, raw, _ = call(a.base, a.key, {
        "model": a.model, "max_tokens": 64, "reasoning_effort": "high",
        "messages": [{"role": "user", "content": "9.11 和 9.9 哪个大？只答哪个大。"}],
    }, a.timeout)
    obj = parse(raw) or {}
    msg = (obj.get("choices") or [{}])[0].get("message") or {}
    out["caps"]["reasoning"] = {
        "status": st,
        "has_reasoning_content": bool(msg.get("reasoning_content")),
        "usage": usage_of(obj),
        "answer": (msg.get("content") or "")[:80],
    }

    # ⑤ 视觉探针：发一张 1x1 PNG
    st, _, raw, _ = call(a.base, a.key, {
        "model": a.model, "max_tokens": 16,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": "这张图里有几个像素？"},
            {"type": "image_url", "image_url": {"url": "data:image/png;base64," + base64.b64encode(PNG_1PX).decode()}},
        ]}],
    }, a.timeout)
    out["caps"]["vision"] = {"status": st, "detail": raw[:200].decode("utf-8", "replace")}

    # ⑥ 工具调用探针
    st, _, raw, _ = call(a.base, a.key, {
        "model": a.model, "max_tokens": 64, "tool_choice": "auto",
        "tools": [{"type": "function", "function": {
            "name": "get_weather", "description": "查天气",
            "parameters": {"type": "object", "properties": {"city": {"type": "string"}},
                           "required": ["city"]}}}],
        "messages": [{"role": "user", "content": "北京今天天气如何？请调用工具。"}],
    }, a.timeout)
    obj = parse(raw) or {}
    tc = ((obj.get("choices") or [{}])[0].get("message") or {}).get("tool_calls") or []
    out["caps"]["tools"] = {"status": st, "n_tool_calls": len(tc),
                            "shape": (tc[0].get("function") or {}).get("name") if tc else None}

    # ⑦ 确定性：temperature=0 跑两次，比 sha256
    digests = []
    for _ in range(2):
        st, _, raw, _ = call(a.base, a.key, {
            "model": a.model, "temperature": 0, "max_tokens": 48,
            "messages": [{"role": "user", "content": "用一句话解释什么是哈希表。"}],
        }, a.timeout)
        digests.append(hashlib.sha256(text_of(parse(raw)).encode()).hexdigest()[:16])
    out["misc"]["determinism_t0"] = {"digests": digests, "identical": digests[0] == digests[1]}

    # ⑧ 知识截止探针（只记录，人工对比）
    st, _, raw, _ = call(a.base, a.key, {
        "model": a.model, "max_tokens": 96,
        "messages": [{"role": "user", "content": "你的训练数据截止到哪一天？请只回答日期。"}],
    }, a.timeout)
    out["misc"]["cutoff_selfreport"] = {"status": st, "answer": text_of(parse(raw))[:120]}

    # 汇总打印
    print(f"\n模型指纹体检  base={a.base}  model={a.model}\n" + "-" * 66)
    for name, _ in PROBES:
        p = out["probes"][name]
        u = p.get("usage") or {}
        print(f"  tokenizer[{name:<5}] chars={p['chars']:<4} prompt_tokens={u.get('prompt_tokens')} status={p['status']}")
    print(f"  meta          model_echo={out['meta'].get('model_echo')!r} fingerprint={out['meta'].get('system_fingerprint')!r}")
    print(f"  max_tokens 探针 status={out['caps']['max_tokens_probe']['status']} detail={out['caps']['max_tokens_probe']['detail'][:110]!r}")
    print(f"  reasoning     status={out['caps']['reasoning']['status']} reasoning_content={out['caps']['reasoning']['has_reasoning_content']} usage={out['caps']['reasoning']['usage']}")
    print(f"  vision        status={out['caps']['vision']['status']}")
    print(f"  tools         status={out['caps']['tools']['status']} n={out['caps']['tools']['n_tool_calls']} name={out['caps']['tools']['shape']}")
    print(f"  determinism   {out['misc']['determinism_t0']['digests']} identical={out['misc']['determinism_t0']['identical']}")
    print(f"  cutoff(自述)  {out['misc']['cutoff_selfreport']['answer']!r}")
    print("-" * 66)
    if a.json:
        with open(a.json, "w", encoding="utf-8") as f:
            json.dump(out, f, ensure_ascii=False, indent=2)
        print(f"  已写入 {a.json}（官方/上游各跑一次，然后 diff）")


if __name__ == "__main__":
    sys.exit(main())

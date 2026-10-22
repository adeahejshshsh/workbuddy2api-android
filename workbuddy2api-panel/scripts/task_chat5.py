#!/usr/bin/env python3
"""一次性任务脚本：chat_5（和 AI 聊天 5 次，+100 积分 +5 能量）.

通过 {billing}/v2/report 上报 chat_request_send 事件完成进度累计。
照抄 report.go 的 chatRequestEvent 完整形状（含 userId，缺则静默丢弃）。
默认 dry-run，--yes 才发真实上报；默认补到 5 次，支持 --count N。

用法
  python3 task_chat5.py <uid前缀>                # dry-run
  python3 task_chat5.py <uid前缀> --yes          # 补齐到 5 次
  python3 task_chat5.py <uid前缀> --yes --count 2  # 只补 2 次
"""
import re
import sys, os, argparse

def _uid_prefix(path):
    """从 workbuddy-<uid>.json 取 uid 前 8 位。

    此前用 os.path.basename(p)[10:18] 硬切（报告 #24）：文件名不是
    workbuddy-<uid>.json 形态（如 workbuddy.json）时得到空串，load_auth 直接
    SystemExit 中断整批。这里按前缀匹配取，取不到返回空串由调用方跳过。
    """
    m = re.search(r"workbuddy-([0-9A-Za-z]+)", os.path.basename(path))
    return m.group(1)[:8] if m else ""

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_common as tc

TASK_CODE = "chat_5"
TARGET = 5


def main():
    ap = argparse.ArgumentParser(description="chat_5 任务：上报 N 次对话完成 5/5")
    ap.add_argument("account", help="uid 前缀，或 ALL")
    ap.add_argument("--yes", action="store_true", help="确认执行写操作")
    ap.add_argument("--count", type=int, default=0,
                    help="本次上报条数（默认自动补到 5 次）")
    ap.add_argument("--gap", type=float, default=1.05)
    a = ap.parse_args()

    prefixes = []
    if a.account.upper() == "ALL":
        import glob
        prefixes = [_uid_prefix(p)
                    for p in sorted(glob.glob(tc.AUTHS + "/workbuddy-*.json"))]
    else:
        prefixes = [a.account]

    for pre in prefixes:
        c = tc.load_auth(pre)
        print(f"== {c['uid'][:8]} ({c['nick']}) ==")
        try:
            st = tc.task_status(c, TASK_CODE)
        except Exception as e:
            print(f"  [skip] list_tasks 失败: {e}")
            continue
        if st is None:
            print(f"  [skip] 无 {TASK_CODE} 任务")
            continue
        ast = st.get("accept_status")
        prog = (st.get("progress") or {})
        cur = prog.get("current", 0)
        target = prog.get("target", TARGET)
        if ast == "claimed" or cur >= target:
            print(f"  [skip] 已完成 {cur}/{target} (accept_status={ast})")
            continue
        need = max(0, target - cur)
        if a.count > 0:
            need = min(need, a.count)
        if need <= 0:
            print("  [skip] 无需上报")
            continue
        if not a.yes:
            print(f"  [dry-run] 将上报 {need} 条 chat_request_send "
                  f"(当前 {cur}/{target}，加 --yes 生效)")
            continue
        results = tc.report_activity(c, count=need, gap=a.gap)
        # 回读确认
        st2 = tc.task_status(c, TASK_CODE)
        prog2 = (st2.get("progress") or {}) if st2 else {}
        print(f"  report x{need} -> {results}")
        print(f"  回读 {TASK_CODE}: {prog2.get('current', 0)}/{prog2.get('target', target)} "
              f"accept_status={st2.get('accept_status') if st2 else '?'}")


if __name__ == "__main__":
    main()
// logexport.go 面板运行日志的按区间导出（GET /panel/api/logs/export）。
//
// 动机：日志落盘之后，「把最近三天发我」就是运维最常要的动作。没有导出，人只能
// SSH 上去翻文件——而面板存在的意义就是不必 SSH。
//
// 导出的是**落盘文件**（按天 JSONL，原样打包），不是内存环：
//   - 内存环只有 500/频道，导出它等于导出一份已经被冲掉的历史；
//   - 事故复盘要的是原始行，重新序列化会引入一次口径漂移（字段名/时间格式）。
//
// 区间口径：
//
//	?days=N           最近 N 天（含今天），1..retentionDays，缺省 3
//	?from=&to=        RFC3339 精确区间（同时给出时优先于 days）
//
// 响应 application/zip，文件名 panel-logs-<from>_<to>.zip。
package panel

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

// exportDefaultDays 缺省导出区间（天）。取 3 而不是 7：多数复盘看的是「前天到现在」，
// 7 天的包在请求体量大时能到几十 MB，浏览器下载体验差。
const exportDefaultDays = 3

// logsExportHandler 打包指定区间内的落盘日志。
func (p *Panel) logsExportHandler(w http.ResponseWriter, r *http.Request) {
	p.logs.mu.Lock()
	store := p.logs.store
	p.logs.mu.Unlock()
	if store == nil {
		writeErr(w, http.StatusNotImplemented, "日志落盘未启用（检查 logging.panel_log_enabled 与数据目录权限）")
		return
	}

	now := cst.Now()
	q := r.URL.Query()
	from, to := now.AddDate(0, 0, -exportDefaultDays), now
	if raw := strings.TrimSpace(q.Get("days")); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days <= 0 {
			writeErr(w, http.StatusBadRequest, "days 需为正整数")
			return
		}
		// 上限取保留期：要更早的数据得先改配置，不能靠一个查询参数绕过保留策略。
		if maxDays := store.retentionDays; maxDays > 0 && days > maxDays {
			days = maxDays
		}
		from = now.AddDate(0, 0, -days)
	}
	if raw := strings.TrimSpace(q.Get("from")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "from 需为 RFC3339（如 2026-11-09T00:00:00+08:00）")
			return
		}
		from = t
	}
	if raw := strings.TrimSpace(q.Get("to")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "to 需为 RFC3339")
			return
		}
		to = t
	}
	if !to.After(from) {
		writeErr(w, http.StatusBadRequest, "to 必须晚于 from")
		return
	}

	// 先落盘到临时文件再整体回传：zip 是流式格式，边写边发时若中途出错，客户端会
	// 拿到一个**看起来完整**的坏包（zip 目录在尾部）。先落盘能把失败变成 HTTP 500。
	tmp, err := os.CreateTemp("", "panel-logs-*.zip")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建导出文件: "+err.Error())
		return
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	files, err := store.Export(tmp, from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "打包日志: "+err.Error())
		return
	}
	if files == 0 {
		// 空区间给明确答复，而不是一个只有空目录的 zip——后者会被当成"导出成功但没内容"。
		writeErr(w, http.StatusNotFound, "该区间没有落盘日志（可能是保留期外，或落盘刚启用）")
		return
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		writeErr(w, http.StatusInternalServerError, "读取导出文件: "+err.Error())
		return
	}
	info, err := tmp.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取导出文件: "+err.Error())
		return
	}
	name := "panel-logs-" + from.In(cst.Loc).Format("20060102") + "_" + to.In(cst.Loc).Format("20060102") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	// 下载不缓冲：大包直接 sendfile 语义，别先读进内存。
	if _, err := tmp.Seek(0, 0); err == nil {
		http.ServeContent(w, r, name, info.ModTime(), tmp)
		return
	}
	log.Printf("WARN: panel: 日志导出回传失败: %v", err)
}

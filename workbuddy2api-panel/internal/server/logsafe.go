package server

import "strings"

// logsafe.go 日志字段清洗（W2-07）。
//
// 背景：model 名来自客户端请求体，只做 JSON 解析、不做字符集校验就进了
// log.Printf("... model=%s ...")。两类事故：
//   - 一个 "\n" 就能在日志里伪造一整行（伪造 ERR/审计行，grep 排障直接被带偏）；
//   - 单个 model 字段可以塞几十 MB（body 上限 32MB），一行日志把磁盘/采集器刷满。
//
// 只清洗**写日志**用的副本，不动业务值：model 名本身要参与别名解析、选号与
// 出站重写，那里改字符会改变语义。

// logModelMaxRunes 日志里模型名的 rune 上限。真实模型名很少超过 64 字符
// （带 realm 前缀与版本号也远小于 120），超出的部分对排障没有价值。
const logModelMaxRunes = 120

// logModel 返回可安全写入单行日志的模型名副本：剥掉所有控制字符
// （\n \r \t、C0/C1、U+2028/2029 等换行类分隔符），并限制长度。
func logModel(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			continue
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			continue
		case r == '\u2028' || r == '\u2029':
			continue
		}
		if n >= logModelMaxRunes {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

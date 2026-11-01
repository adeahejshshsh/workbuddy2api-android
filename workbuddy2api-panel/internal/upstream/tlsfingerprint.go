// tlsfingerprint.go 出站 TLS ClientHello 指纹复刻（Node 22 / OpenSSL 3.x）。
//
// 背景：请求头做到 100% 一致也只能骗过 HTTP 层——TLS 握手在 HTTP 之前就完成了。
// Go 的 crypto/tls 有自己一套固定形态（13 个 cipher、混入 X25519MLKEM768 等
// 后量子曲线、带 SCT(18) 与 status_request(5)），与官方客户端的 Node 栈完全不同，
// 现代风控先看 JA3/JA4 再看 header 时一撞就露。
//
// 真机证据（2026-11-01 抓包，MITM 记录完整 ClientHello 原始字节）：
//
//	国内 www.workbuddy.cn   JA3 71dc8c533dd919ae9f4963224a4ba8fd
//	国际 www.workbuddy.ai   JA3 71dc8c533dd919ae9f4963224a4ba8fd
//	                        JA4 t13d0a1200_5d04281c6031_b76c5ee2507d
//
// **两版完全同值**——一套 spec 覆盖 CN 与 GL，不需要按 realm 分档。
// 官方 /v2/chat/completions、/v2/report、/v3/config、billing/* 全部走这套
// （420 次握手逐字节一致）。
//
// 关键前提：官方 chat 走的是 **Node 主进程**，不是渲染器 Chromium。所以它的
// ClientHello **不带 ALPN**——这与本仓「禁 h2 治半死连接」的既有策略天然一致，
// 不存在「要指纹像 Chromium 就得分 ALPN 带 h2」的那种取舍。
//
// 渲染器另有一套 Chromium 指纹（16 cipher / 18 扩展 / ALPN=h2），本仓出站不发
// 渲染器形态的请求，故不复刻。
package upstream

import (
	"crypto/tls"
	"fmt"
	"strings"

	utls "github.com/refraction-networking/utls"
)

// TLS 指纹档位。
const (
	// TLSFingerprintNode 复刻官方 Node/OpenSSL 形态（生产默认）。
	TLSFingerprintNode = "node"
	// TLSFingerprintGo 用 crypto/tls 原生形态（排障/回滚用）。
	TLSFingerprintGo = "go"
)

// 真机实测常量（抓包取得，用于回归断言，不是推算值）。
const (
	// OfficialJA3MD5 官方 chat 链路 ClientHello 的 JA3 md5。
	OfficialJA3MD5 = "71dc8c533dd919ae9f4963224a4ba8fd"
	// OfficialJA3 官方 chat 链路 ClientHello 的 JA3 原文。
	OfficialJA3 = "771," +
		"4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53," +
		"0-23-65281-10-11-35-13-51-45-43," +
		"29-23-24," +
		"0"
	// OfficialJA4 官方 chat 链路的 JA4。
	OfficialJA4 = "t13d0a1200_5d04281c6031_b76c5ee2507d"
)

// officialCipherSuites 官方 ClientHello 的 18 个 cipher，**顺序即真机顺序**。
// 与 Go 默认的差异不只是数量：TLS1.3 三个套件（0x1301/1302/1303）在这里排最前，
// 而 Go 把它们排在末尾。
var officialCipherSuites = []uint16{
	tls.TLS_AES_128_GCM_SHA256,                        // 0x1301 4865
	tls.TLS_AES_256_GCM_SHA384,                        // 0x1302 4866
	tls.TLS_CHACHA20_POLY1305_SHA256,                  // 0x1303 4867
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,         // 0xc02f 49199
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,       // 0xc02b 49195
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,         // 0xc030 49200
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,       // 0xc02c 49196
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,         // 0xc027 49191
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256, // 0xcca9 52393
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,   // 0xcca8 52392
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,          // 0xc009 49161
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,            // 0xc013 49171
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,          // 0xc00a 49162
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,            // 0xc014 49172
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,               // 0x009c 156
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384,               // 0x009d 157
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,                  // 0x002f 47
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,                  // 0x0035 53
}

// officialSigAlgs 官方 signature_algorithms 的 9 项。
//
// JA3 看不到这组值，但 JA4 的第三段完全由它决定——少一个（ed25519/ed448/
// pss_pss 官方都不发）或多一个，JA4 立刻跑偏。所以必须逐字节照抄。
var officialSigAlgs = []utls.SignatureScheme{
	0x0403, // ecdsa_secp256r1_sha256
	0x0804, // rsa_pss_rsae_sha256
	0x0401, // rsa_pkcs1_sha256
	0x0503, // ecdsa_secp384r1_sha384
	0x0805, // rsa_pss_rsae_sha384
	0x0501, // rsa_pkcs1_sha384
	0x0806, // rsa_pss_rsae_sha512
	0x0601, // rsa_pkcs1_sha512
	0x0201, // rsa_pkcs1_sha1
}

// officialHelloSpec 复刻官方 Node/OpenSSL ClientHello。
//
// 扩展顺序即真机顺序：SNI → EMS → renegotiation → groups → ecpf →
// session_ticket → sigalgs → key_share → psk_modes → supported_versions。
// 顺序错 JA3 的扩展段就对不上（JA3 用扩展出现序，JA4 用排序后）。
func officialHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:         tls.VersionTLS12,
		TLSVersMax:         tls.VersionTLS13,
		CipherSuites:       officialCipherSuites,
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},                      // 0
			&utls.UtlsExtendedMasterSecretExtension{}, // 23
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient}, // 65281
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519,    // 29
				utls.CurveP256, // 23
				utls.CurveP384, // 24
			}}, // 10
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},                        // 11
			&utls.SessionTicketExtension{},                                                    // 35
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: officialSigAlgs}, // 13
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519},
			}}, // 51
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}}, // 45
			&utls.SupportedVersionsExtension{Versions: []uint16{
				tls.VersionTLS13,
				tls.VersionTLS12,
			}}, // 43
		},
	}
}

// useNodeFingerprint 该次拨号是否走复刻指纹。空值按生产默认（node）处理，
// 只有显式 "go" 才回退原生——回滚只需要改配置，不用改代码。
func useNodeFingerprint(fp string) bool {
	return strings.ToLower(strings.TrimSpace(fp)) != TLSFingerprintGo
}

// validateTLSFingerprint 校验配置取值，返回规范化后的值。
func validateTLSFingerprint(fp string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(fp)); v {
	case "", TLSFingerprintNode:
		return TLSFingerprintNode, nil
	case TLSFingerprintGo:
		return TLSFingerprintGo, nil
	default:
		return "", fmt.Errorf("upstream.tls_fingerprint 取值 %q 无效（可选 node / go）", fp)
	}
}

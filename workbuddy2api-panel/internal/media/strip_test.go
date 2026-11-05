package media

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"runtime"
	"testing"
)

// stripTestImg 造一张 w×h 的测试图：y < contentH 是黑白棋盘（每行亮度标准差远大于
// rowStdThreshold），其余行铺 blank 色。用它模拟「画面 + 底部纯白留白」。
func stripTestImg(w, h, contentH int, blank color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if y < contentH {
				if (x+y)%2 == 0 {
					img.SetRGBA(x, y, color.RGBA{R: 12, G: 24, B: 36, A: 255})
				} else {
					img.SetRGBA(x, y, color.RGBA{R: 240, G: 236, B: 232, A: 255})
				}
				continue
			}
			img.SetRGBA(x, y, blank)
		}
	}
	return img
}

func stripTestPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func stripTestJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func stripDecodeConfig(t *testing.T, raw []byte) image.Config {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	return cfg
}

func TestStripCropsBlankBottom(t *testing.T) {
	raw := stripTestPNG(t, stripTestImg(40, 120, 80, color.RGBA{R: 255, G: 255, B: 255, A: 255}))
	res, err := Strip(raw, Options{MinBlank: 20})
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if res.Boundary != 80 || res.BlankPx != 40 || res.OutHeight != 80 {
		t.Fatalf("边界/留白/输出高度不对：boundary=%d blank=%d out_h=%d", res.Boundary, res.BlankPx, res.OutHeight)
	}
	if !res.Safe {
		t.Fatal("40px 纯白留白（阈值 20）应判安全")
	}
	if res.Format != "png" {
		t.Fatalf("格式省略时应跟随输入 png：%s", res.Format)
	}
	if res.BlankLum < 250 {
		t.Fatalf("留白平均亮度应接近 255：%.1f", res.BlankLum)
	}
	if cfg := stripDecodeConfig(t, res.Data); cfg.Width != 40 || cfg.Height != 80 {
		t.Fatalf("裁出的图尺寸 %dx%d，应为 40x80", cfg.Width, cfg.Height)
	}
}

func TestStripDefaultThresholds(t *testing.T) {
	// 120px 留白 ≥ 默认安全线 90px，零值 Options 也应判安全。
	raw := stripTestPNG(t, stripTestImg(60, 240, 120, color.RGBA{R: 255, G: 255, B: 255, A: 255}))
	res, err := Strip(raw, Options{})
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if res.MinBlank != DefaultMinBlank || res.WhiteLum != DefaultWhiteLum {
		t.Fatalf("零值应落默认阈值：min_blank=%d white_lum=%.1f", res.MinBlank, res.WhiteLum)
	}
	if !res.Safe || res.OutHeight != 120 {
		t.Fatalf("120px 白留白应安全并裁到 120：safe=%v out_h=%d", res.Safe, res.OutHeight)
	}
}

func TestStripKeepBlank(t *testing.T) {
	raw := stripTestPNG(t, stripTestImg(40, 120, 80, color.RGBA{R: 255, G: 255, B: 255, A: 255}))
	res, err := Strip(raw, Options{MinBlank: 20, KeepBlank: 6})
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if res.OutHeight != 86 {
		t.Fatalf("keep_blank=6 应裁到 80+6=86，实际 %d", res.OutHeight)
	}
}

func TestStripNoBlankIsUnsafe(t *testing.T) {
	// 满幅棋盘：底部没有留白，必须拒绝而不是把画面切掉。
	raw := stripTestPNG(t, stripTestImg(40, 120, 120, color.RGBA{R: 255, G: 255, B: 255, A: 255}))
	res, err := Strip(raw, Options{MinBlank: 20})
	var nb *NoBlankError
	if !errors.As(err, &nb) {
		t.Fatalf("留白为 0 应回 NoBlankError，实际 err=%v", err)
	}
	if res.BlankPx != 0 || res.Safe {
		t.Fatalf("拒绝时也应带实测数字：blank=%d safe=%v", res.BlankPx, res.Safe)
	}
	if nb.Result.BlankPx != 0 {
		t.Fatalf("NoBlankError.Result 应带实测留白，实际 %d", nb.Result.BlankPx)
	}
}

func TestStripGrayBlankIsUnsafe(t *testing.T) {
	// 灰底留白：std 也小，但亮度不够白 → 不能让调用方以为裁掉了白边。
	raw := stripTestPNG(t, stripTestImg(40, 120, 80, color.RGBA{R: 128, G: 128, B: 128, A: 255}))
	res, err := Strip(raw, Options{MinBlank: 20})
	var nb *NoBlankError
	if !errors.As(err, &nb) {
		t.Fatalf("灰底留白应判不安全，实际 err=%v", err)
	}
	if res.BlankPx != 40 {
		t.Fatalf("留白像素应为 40，实际 %d", res.BlankPx)
	}
	if res.BlankLum < 120 || res.BlankLum > 140 {
		t.Fatalf("灰底留白平均亮度应在 128 附近：%.1f", res.BlankLum)
	}
}

func TestStripUniformImage(t *testing.T) {
	raw := stripTestPNG(t, stripTestImg(40, 120, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255}))
	if _, err := Strip(raw, Options{}); !errors.Is(err, ErrUniform) {
		t.Fatalf("整张纯色应回 ErrUniform，实际 %v", err)
	}
}

func TestStripNotImage(t *testing.T) {
	if _, err := Strip([]byte("这不是图片"), Options{}); !errors.Is(err, ErrNotImage) {
		t.Fatalf("非图片字节应回 ErrNotImage，实际 %v", err)
	}
}

// bombPNG 手写一张 w×h 的 1 位灰度全黑 PNG：每行只占 w/8 字节、全零压得极小，
// 解码后却是每像素 1 字节的 image.Gray——几十 KB 的文件能要几百 MB 内存。
func bombPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var out bytes.Buffer
	chunk := func(typ string, data []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(data)))
		out.Write(n[:])
		crc := crc32.NewIEEE()
		crc.Write([]byte(typ))
		crc.Write(data)
		out.WriteString(typ)
		out.Write(data)
		binary.BigEndian.PutUint32(n[:], crc.Sum32())
		out.Write(n[:])
	}
	out.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8] = 1 // 位深 1，颜色类型 0（灰度）
	chunk("IHDR", ihdr)
	var idat bytes.Buffer
	zw := zlib.NewWriter(&idat)
	row := make([]byte, 1+(w+7)/8) // 首字节是行过滤类型 0
	for y := 0; y < h; y++ {
		if _, err := zw.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	chunk("IDAT", idat.Bytes())
	chunk("IEND", nil)
	return out.Bytes()
}

// 像素上限必须在解码之前查：先解码再比，上限就拦不住它要防的那次内存分配。
// 12000×12000 超过 MaxPixels；文件只有几十 KB，完整解码要分配约 144MB。
func TestStripRejectsOversizeBeforeDecode(t *testing.T) {
	raw := bombPNG(t, 12000, 12000)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := Strip(raw, Options{})
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超像素上限应回 ErrTooLarge，实际 %v", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 8<<20 {
		t.Fatalf("拒绝超大图前分配了 %d MB：像素上限没在解码前生效", alloc>>20)
	}
}

func TestStripJPEGFollowsInput(t *testing.T) {
	raw := stripTestJPEG(t, stripTestImg(40, 120, 80, color.RGBA{R: 255, G: 255, B: 255, A: 255}))
	res, err := Strip(raw, Options{MinBlank: 20})
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if res.Format != "jpeg" {
		t.Fatalf("JPEG 输入应输出 jpeg：%s", res.Format)
	}
	if len(res.Data) < 2 || res.Data[0] != 0xFF || res.Data[1] != 0xD8 {
		t.Fatal("输出不是 JPEG 字节流")
	}
	// JPEG 在棋盘/留白交界会有振铃，边界可能上移 1~2 行，允许小幅误差。
	if res.OutHeight < 72 || res.OutHeight > 80 {
		t.Fatalf("JPEG 裁切高度应在 72~80，实际 %d", res.OutHeight)
	}
}

func TestStripFormatOverride(t *testing.T) {
	raw := stripTestPNG(t, stripTestImg(40, 120, 80, color.RGBA{R: 255, G: 255, B: 255, A: 255}))
	res, err := Strip(raw, Options{MinBlank: 20, Format: "jpeg", Quality: 80})
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if res.Format != "jpeg" {
		t.Fatalf("显式 format=jpeg 应输出 jpeg：%s", res.Format)
	}
	if cfg := stripDecodeConfig(t, res.Data); cfg.Height > 80 {
		t.Fatalf("输出高度 %d 超过内容高度", cfg.Height)
	}
}

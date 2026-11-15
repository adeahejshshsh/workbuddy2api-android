package media

// 第二轮代码审计（CODE-AUDIT-FULL.md media 段 / W2-ME-01、W2-ME-02）的回归用例。

import (
	"image"
	"image/color"
	"sync"
	"testing"
)

// genericImage 把具体图像类型藏起来，强制 rowStats 走通用（逐像素 At()）路径。
// 用它和快路径对拍，才能证明快路径没有算错。
type genericImage struct{ image.Image }

// ---------------------------------------------------------------- W2-ME-01 ----

// 审查 W2-ME-01：1 亿像素上限在手机上必然 OOM（解码约 400MB）。
func TestMaxPixelsLoweredForMobile(t *testing.T) {
	if MaxPixels > 25_000_000 {
		t.Fatalf("MaxPixels=%d 仍偏大（按 4 字节/像素算解码缓冲 %d MB）",
			MaxPixels, MaxPixels*4>>20)
	}
	if MaxPixels <= 0 {
		t.Fatal("MaxPixels 必须为正")
	}
}

// 审查 W2-ME-01：裁切不再复制一份整幅 RGBA，而是零分配的 SubImage 视图。
func TestCropImageUsesSubImageView(t *testing.T) {
	src := image.NewYCbCr(image.Rect(0, 0, 40, 60), image.YCbCrSubsampleRatio420)
	r := image.Rect(0, 0, 40, 30)
	got := cropImage(src, r)
	if _, ok := got.(*image.RGBA); ok {
		t.Fatal("YCbCr 输入不该被复制成 RGBA（2500 万像素 = 100MB 额外分配）")
	}
	if got.Bounds() != r {
		t.Fatalf("裁切 bounds=%v want %v", got.Bounds(), r)
	}
	// 视图必须与原图同源（SubImage 的语义）。
	if _, ok := got.(*image.YCbCr); !ok {
		t.Fatalf("应返回 *image.YCbCr 视图，得到 %T", got)
	}
}

// 裁切结果像素必须与「复制到 RGBA」的旧路径一致（不能为了省内存改坏画面）。
func TestCropImagePixelsMatchCopyPath(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			src.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 128, A: 255})
		}
	}
	r := image.Rect(1, 2, 7, 6)
	got := cropImage(src, r)

	old := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			old.Set(x-r.Min.X, y-r.Min.Y, src.At(x, y))
		}
	}
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			gr, gg, gb, ga := got.At(r.Min.X+x, r.Min.Y+y).RGBA()
			or, og, ob, oa := old.At(x, y).RGBA()
			if gr != or || gg != og || gb != ob || ga != oa {
				t.Fatalf("(%d,%d) 视图=%v 复制=%v", x, y,
					[]uint32{gr, gg, gb, ga}, []uint32{or, og, ob, oa})
			}
		}
	}
}

// 并发闸：多张图同时进来必须全部处理完（不丢请求、不死锁）。
func TestStripConcurrent(t *testing.T) {
	if cap(stripSlots) < 1 {
		t.Fatal("并发闸容量必须 >= 1")
	}
	raw := stripTestPNG(t, stripTestImg(40, 120, 80, color.RGBA{R: 255, G: 255, B: 255, A: 255}))
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = Strip(raw, Options{MinBlank: 20})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发调用失败: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------- W2-ME-02 ----

// 审查 W2-ME-02：非 Gray/YCbCr/NRGBA/RGBA 的输入此前走通用路径（每像素一次
// img.At() 接口调用）。这里对拍快路径与通用路径的行统计。
func TestRowStatsFastPathsMatchGeneric(t *testing.T) {
	cases := []struct {
		name string
		img  image.Image
	}{
		{"Paletted", palettedTestImage()},
		{"Gray16", gray16TestImage()},
		{"NRGBA64", nrgba64TestImage()},
		{"RGBA64", rgba64TestImage()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fast := rowStats(c.img)
			slow := rowStats(genericImage{c.img})
			if len(fast) != len(slow) {
				t.Fatalf("行数 %d vs %d", len(fast), len(slow))
			}
			for y := range fast {
				// 快路径从原始字节算、通用路径经 16bit 颜色往返，允许 1 个亮度单位的误差。
				if d := fast[y].mean - slow[y].mean; d > 1 || d < -1 {
					t.Fatalf("第 %d 行 mean 不一致: 快=%f 通用=%f", y, fast[y].mean, slow[y].mean)
				}
				if d := fast[y].std - slow[y].std; d > 1 || d < -1 {
					t.Fatalf("第 %d 行 std 不一致: 快=%f 通用=%f", y, fast[y].std, slow[y].std)
				}
			}
		})
	}
}

func palettedTestImage() image.Image {
	pal := color.Palette{
		color.RGBA{R: 255, G: 255, B: 255, A: 255},
		color.RGBA{R: 10, G: 20, B: 30, A: 255},
		color.RGBA{R: 200, G: 120, B: 60, A: 255},
	}
	m := image.NewPaletted(image.Rect(0, 0, 6, 4), pal)
	for i := range m.Pix {
		m.Pix[i] = uint8(i % len(pal))
	}
	return m
}

func gray16TestImage() image.Image {
	m := image.NewGray16(image.Rect(0, 0, 6, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 6; x++ {
			m.SetGray16(x, y, color.Gray16{Y: uint16((x*7 + y*11) * 0x0101)})
		}
	}
	return m
}

func nrgba64TestImage() image.Image {
	m := image.NewNRGBA64(image.Rect(0, 0, 6, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 6; x++ {
			// 含一个半透明像素：预乘口径必须与 img.At().RGBA() 一致。
			a := uint16(0xffff)
			if x == 0 && y == 0 {
				a = 0x8000
			}
			m.SetNRGBA64(x, y, color.NRGBA64{
				R: uint16(x * 0x1111), G: uint16(y * 0x2222), B: 0x3333, A: a,
			})
		}
	}
	return m
}

func rgba64TestImage() image.Image {
	m := image.NewRGBA64(image.Rect(0, 0, 6, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 6; x++ {
			m.SetRGBA64(x, y, color.RGBA64{
				R: uint16(x * 0x1111), G: uint16(y * 0x2222), B: 0x3333, A: 0xffff,
			})
		}
	}
	return m
}

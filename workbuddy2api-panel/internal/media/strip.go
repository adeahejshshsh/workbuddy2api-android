// Package media 对上游产出的图片做本地后处理。
//
// 目前只有一件事：裁掉右下角水印。WorkBuddy / 混元生图会在成品右下角印一行
// 「AI生成 / WORKBUDD>.」的半透明白字（2026-11 实测：固定贴右边、下边各约 10px，
// 高度 51px（1024~1536 边长档）到约 80px（1344x1792 档））。因此只要图片底部真有
// 一条留白，把这条留白整条裁掉，水印必然一起消失——不需要识别水印形状，也不需要
// 知道它的确切尺寸。
//
// 为什么判定条件要求「行标准差小 **而且** 接近纯白」：整幅竖向渐变（天空、墙面、
// 纯色背景）的每一行标准差同样很小，只看标准差会把正常画面当留白裁掉。加上亮度
// 下限，才能把「作者有意留的白边」和「画面本身很平」分开。这也正是提示词里要写
// 「下方三分之一纯白留白」的原因：模型给得出 25~38% 的真白边时这条链路才成立。
package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
)

const (
	// DefaultMinBlank 判定为「可裁留白」的最小高度（像素）。
	// 水印最高约 80px + 下边距 10px，所以从 90px 起步：比这更矮的留白裁不干净。
	DefaultMinBlank = 90
	// DefaultWhiteLum 留白区平均亮度下限（0-255）。实测白边 253~255，留 5 点余量。
	DefaultWhiteLum = 248.0
	// DefaultJPEGQuality opts.Quality 为 0 时的 JPEG 质量。
	DefaultJPEGQuality = 92
	// MaxPixels 单张图的像素上限，防解码炸内存。
	//
	// 审查 W2-ME-01：原值 1 亿（解码后约 400MB）在 Android 设备上必然被 OOM
	// killer 直接杀掉（Go 的 OOM 不可 recover）。按部署形态下调到 2500 万：
	// 解码缓冲约 75~100MB，且裁切不再额外复制整幅缓冲（见 cropImage）。
	MaxPixels = 25_000_000
	// rowStdThreshold 判「这一行有内容」的行标准差阈值（0-255 亮度）。
	// 实测纯白留白区的行标准差 0.1~0.7，画面内容行普遍 > 2。
	rowStdThreshold = 2.0
)

// stripSlots 同时处理的图片数上限（审查 W2-ME-01）。
//
// 像素上限只挡单张，挡不住「并发多张同时解码」——N 张 25M 像素图并发就是 N 倍
// 内存。图像后处理是 CPU/内存双密集型，2 个并发在手机/小内存容器上是安全水位；
// 单张亚秒级，排队的请求等一会儿即可，不会饿死。
var stripSlots = make(chan struct{}, 2)

// ErrNotImage 输入不是本包能解的图片（只支持 PNG / JPEG）。
var ErrNotImage = errors.New("不支持的图片格式：只认 PNG / JPEG")

// ErrTooLarge 图片像素数超过 MaxPixels。
var ErrTooLarge = errors.New("图片过大：超过像素上限")

// ErrUniform 整张图都是同一颜色（没有可裁的内容边界）。
var ErrUniform = errors.New("整张图都是纯色，没有可裁的内容边界")

// NoBlankError 底部没有足够的纯白留白，裁了会切到画面。
//
// 带上 Result 是为了让调用方拿到实测数字（底部到底有多少像素、亮度多少），
// 好据此决定是重画、还是先让 edit 模型把下方涂白。
type NoBlankError struct {
	Result Result
}

func (e *NoBlankError) Error() string {
	return fmt.Sprintf("底部留白不足：只有 %dpx（需 ≥%dpx，且平均亮度 ≥%.0f），裁掉会切到画面",
		e.Result.BlankPx, e.Result.MinBlank, e.Result.WhiteLum)
}

// Options 裁切参数。零值即默认（90px 留白断言 + 纯白判定 + 跟随输入格式）。
type Options struct {
	MinBlank  int     // <=0 用 DefaultMinBlank
	WhiteLum  float64 // <=0 用 DefaultWhiteLum
	KeepBlank int     // 裁完在内容下方保留的留白像素（>=0）
	Format    string  // "" = 跟随输入；"png" / "jpeg"
	Quality   int     // JPEG 质量 1-100；<=0 用 DefaultJPEGQuality
}

// Result 一次裁切的结果（无论成功失败都会填好数字）。
type Result struct {
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	OutWidth   int     `json:"out_width"`
	OutHeight  int     `json:"out_height"`
	Boundary   int     `json:"boundary"`    // 裁切线：保留 [0,boundary) 行
	BlankPx    int     `json:"blank_px"`    // 底部纯白留白高度
	BlankRatio float64 `json:"blank_ratio"` // 留白占原图高度比例
	BlankLum   float64 `json:"blank_lum"`   // 留白区平均亮度
	MinBlank   int     `json:"min_blank"`
	WhiteLum   float64 `json:"white_lum"`
	Safe       bool    `json:"safe"`   // 是否判定为可安全裁切
	Format     string  `json:"format"` // 输出格式 png/jpeg
	Data       []byte  `json:"-"`      // 裁好的图片字节（失败时为空）
}

// Strip 裁掉图片底部的纯白留白。
//
// 返回的 Result 无论 err 是否为 nil 都带实测数字；err 为 *NoBlankError 时说明底部
// 留白不够（此时 Result.Data 为空，调用方应让用户先补白边再裁）。
func Strip(raw []byte, opts Options) (Result, error) {
	// 并发闸（审查 W2-ME-01）：见 stripSlots 注释。
	stripSlots <- struct{}{}
	defer func() { <-stripSlots }()

	if opts.MinBlank <= 0 {
		opts.MinBlank = DefaultMinBlank
	}
	if opts.WhiteLum <= 0 {
		opts.WhiteLum = DefaultWhiteLum
	}
	if opts.KeepBlank < 0 {
		opts.KeepBlank = 0
	}
	if opts.Quality <= 0 {
		opts.Quality = DefaultJPEGQuality
	}

	// 先只读文件头拿尺寸、查像素上限，再完整解码：解码按声明的尺寸一次分配像素缓冲，
	// 几十 KB 的高压缩 PNG 就能声明上亿像素，上限放在解码之后拦不住这次分配。
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrNotImage, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Result{}, fmt.Errorf("%w: 尺寸为 0", ErrNotImage)
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return Result{}, ErrTooLarge
	}
	img, inFormat, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrNotImage, err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	res := Result{
		Width: w, Height: h, MinBlank: opts.MinBlank, WhiteLum: opts.WhiteLum,
		Format: chooseFormat(opts.Format, inFormat),
	}

	stats := rowStats(img)
	// boundary 取「最下面一行有内容」的下方一行：0 表示整张图没有任何内容边界
	// （纯色/纯渐变），h 表示内容顶到底、没有留白。
	boundary := 0
	for y := h - 1; y >= 0; y-- {
		if stats[y].std >= rowStdThreshold {
			boundary = y + 1
			break
		}
	}
	if boundary == 0 {
		return res, ErrUniform
	}
	blank := h - boundary
	res.Boundary = boundary
	res.BlankPx = blank
	res.BlankRatio = float64(blank) / float64(h)
	if blank > 0 {
		res.BlankLum = meanLumOverStats(stats[boundary:])
	} else {
		res.BlankLum = meanLumOverStats(stats)
	}

	safe := blank >= opts.MinBlank && res.BlankLum >= opts.WhiteLum
	res.Safe = safe
	if !safe {
		return res, &NoBlankError{Result: res}
	}

	keep := opts.KeepBlank
	cropH := boundary + keep
	if cropH > h {
		cropH = h
	}
	if cropH < 1 {
		cropH = 1
	}
	cropped := cropImage(img, image.Rect(b.Min.X, b.Min.Y, b.Min.X+w, b.Min.Y+cropH))
	out, err := encode(cropped, res.Format, opts.Quality)
	if err != nil {
		return res, err
	}
	res.Data = out
	res.OutWidth = w
	res.OutHeight = cropH
	return res, nil
}

// rowStat 一行亮度统计。
type rowStat struct {
	mean float64
	std  float64
}

// rowStats 逐行算亮度均值与标准差。带 stdlib 常见图像类型的快路径
// （JPEG 解出来是 *image.YCbCr，直接读 Y 平面，不逐像素转颜色）。
func rowStats(img image.Image) []rowStat {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]rowStat, h)

	switch m := img.(type) {
	case *image.Gray:
		for y := 0; y < h; y++ {
			off := (y+b.Min.Y-m.Rect.Min.Y)*m.Stride + (b.Min.X - m.Rect.Min.X)
			out[y] = statsFromBytes(m.Pix[off : off+w])
		}
		return out
	case *image.YCbCr:
		for y := 0; y < h; y++ {
			off := m.YOffset(b.Min.X, b.Min.Y+y)
			out[y] = statsFromBytes(m.Y[off : off+w])
		}
		return out
	case *image.NRGBA:
		for y := 0; y < h; y++ {
			off := (y+b.Min.Y-m.Rect.Min.Y)*m.Stride + (b.Min.X-m.Rect.Min.X)*4
			out[y] = statsRGB(m.Pix[off : off+w*4])
		}
		return out
	case *image.RGBA:
		for y := 0; y < h; y++ {
			off := (y+b.Min.Y-m.Rect.Min.Y)*m.Stride + (b.Min.X-m.Rect.Min.X)*4
			out[y] = statsRGB(m.Pix[off : off+w*4])
		}
		return out
	case *image.Paletted:
		// 审查 W2-ME-02：调色板图（PNG-8 / GIF）此前落到通用路径，每像素一次
		// img.At() 接口调用 + 16bit 转换。查表即可，成本降到约 1/10。
		for y := 0; y < h; y++ {
			off := (y+b.Min.Y-m.Rect.Min.Y)*m.Stride + (b.Min.X - m.Rect.Min.X)
			out[y] = statsPaletted(m.Pix[off:off+w], m.Palette)
		}
		return out
	case *image.Gray16:
		for y := 0; y < h; y++ {
			off := (y+b.Min.Y-m.Rect.Min.Y)*m.Stride + (b.Min.X-m.Rect.Min.X)*2
			out[y] = statsGray16(m.Pix[off : off+w*2])
		}
		return out
	case *image.RGBA64:
		// 预乘 alpha：与通用路径 img.At().RGBA() 同口径。
		for y := 0; y < h; y++ {
			off := (y+b.Min.Y-m.Rect.Min.Y)*m.Stride + (b.Min.X-m.Rect.Min.X)*8
			out[y] = statsRGB16(m.Pix[off:off+w*8], false)
		}
		return out
	case *image.NRGBA64:
		// 非预乘 alpha：statsRGB16 内部按 alpha 预乘，口径与 RGBA() 一致。
		for y := 0; y < h; y++ {
			off := (y+b.Min.Y-m.Rect.Min.Y)*m.Stride + (b.Min.X-m.Rect.Min.X)*8
			out[y] = statsRGB16(m.Pix[off:off+w*8], true)
		}
		return out
	}

	// 通用路径：逐像素取灰度（慢，但不是主路径）。
	for y := 0; y < h; y++ {
		var sum, sumSq float64
		for x := 0; x < w; x++ {
			lum := float64(grayAt(img, b.Min.X+x, b.Min.Y+y))
			sum += lum
			sumSq += lum * lum
		}
		out[y] = finishRow(sum, sumSq, w)
	}
	return out
}

// statsFromBytes 单通道（灰度 / Y 平面）行的统计。
func statsFromBytes(pix []byte) rowStat {
	var sum, sumSq float64
	for _, v := range pix {
		lum := float64(v)
		sum += lum
		sumSq += lum * lum
	}
	return finishRow(sum, sumSq, len(pix))
}

// statsRGB 4 字节/像素（RGBA 或 NRGBA）行的统计，按 ITU-R BT.601 亮度。
func statsRGB(pix []byte) rowStat {
	var sum, sumSq float64
	n := len(pix) / 4
	for i := 0; i < n; i++ {
		j := i * 4
		lum := 0.299*float64(pix[j]) + 0.587*float64(pix[j+1]) + 0.114*float64(pix[j+2])
		sum += lum
		sumSq += lum * lum
	}
	return finishRow(sum, sumSq, n)
}

// statsPaletted 调色板图（1 字节/像素索引）行的统计。
func statsPaletted(pix []byte, pal color.Palette) rowStat {
	var sum, sumSq float64
	for _, idx := range pix {
		var r, g, b uint8
		if int(idx) < len(pal) {
			cr, cg, cb, _ := pal[idx].RGBA()
			r, g, b = uint8(cr>>8), uint8(cg>>8), uint8(cb>>8)
		}
		lum := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
		sum += lum
		sumSq += lum * lum
	}
	return finishRow(sum, sumSq, len(pix))
}

// statsGray16 16 位灰度图（2 字节/像素，大端）行的统计，折算到 0-255。
func statsGray16(pix []byte) rowStat {
	var sum, sumSq float64
	n := len(pix) / 2
	for i := 0; i < n; i++ {
		lum := float64(binary.BigEndian.Uint16(pix[i*2:])) / 257.0
		sum += lum
		sumSq += lum * lum
	}
	return finishRow(sum, sumSq, n)
}

// statsRGB16 16 位 RGBA（8 字节/像素，大端）行的统计，折算到 0-255。
// premult=true 表示源是非预乘（NRGBA64），按 alpha 预乘以对齐 img.At().RGBA() 口径。
func statsRGB16(pix []byte, premult bool) rowStat {
	var sum, sumSq float64
	n := len(pix) / 8
	for i := 0; i < n; i++ {
		j := i * 8
		r := float64(binary.BigEndian.Uint16(pix[j:]))
		g := float64(binary.BigEndian.Uint16(pix[j+2:]))
		b := float64(binary.BigEndian.Uint16(pix[j+4:]))
		if premult {
			if a := float64(binary.BigEndian.Uint16(pix[j+6:])); a < 65535 {
				r, g, b = r*a/65535, g*a/65535, b*a/65535
			}
		}
		lum := (0.299*r + 0.587*g + 0.114*b) / 257.0
		sum += lum
		sumSq += lum * lum
	}
	return finishRow(sum, sumSq, n)
}

func finishRow(sum, sumSq float64, n int) rowStat {
	if n <= 0 {
		return rowStat{}
	}
	mean := sum / float64(n)
	variance := sumSq/float64(n) - mean*mean
	if variance < 0 { // 浮点误差，别让 NaN 传下去
		variance = 0
	}
	return rowStat{mean: mean, std: math.Sqrt(variance)}
}

// grayAt 通用路径取一点灰度。
func grayAt(img image.Image, x, y int) uint8 {
	r, g, b, _ := img.At(x, y).RGBA() // 16-bit
	return uint8((0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)) / 257.0)
}

func meanLumOverStats(rows []rowStat) float64 {
	if len(rows) == 0 {
		return 0
	}
	var sum float64
	for _, r := range rows {
		sum += r.mean
	}
	return sum / float64(len(rows))
}

// cropImage 裁出 r 区域。
//
// 审查 W2-ME-01：此前无条件 image.NewRGBA + draw.Draw —— 裁切后仍是整幅宽度，
// 等于在解码缓冲之外再分配一份 4 字节/像素的整图（2500 万像素就是 100MB），
// 手机上足以被 OOM killer 杀掉。stdlib 的每个图像类型都实现了 SubImage，它只是
// 换 Bounds 的零分配视图，jpeg/png 编码器都能直接吃（JPEG 还能走 YCbCr 快路径，
// 少一次 RGB 往返）。
func cropImage(img image.Image, r image.Rectangle) image.Image {
	if sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return sub.SubImage(r)
	}
	// 非 stdlib 的自定义 Image 实现：退回复制路径（保持旧行为）。
	dst := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(dst, dst.Bounds(), img, r.Min, draw.Src)
	return dst
}

func chooseFormat(want, in string) string {
	format := want
	if format == "" {
		format = in // image.Decode 回报的格式名（png / jpeg）
	}
	if format == "jpg" {
		format = "jpeg"
	}
	if format != "png" && format != "jpeg" {
		format = "png"
	}
	return format
}

func encode(img image.Image, format string, quality int) ([]byte, error) {
	var buf bytes.Buffer
	switch format {
	case "jpeg":
		if quality < 1 {
			quality = 1
		}
		if quality > 100 {
			quality = 100
		}
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
	default:
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

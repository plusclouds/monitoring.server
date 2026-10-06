package camera

import (
	"bytes"
	"image"
	_ "image/jpeg" // snapshots are JPEG
	_ "image/png"  // some cameras send PNG
	"math"
	"math/bits"
)

// Image measures, all from a grayscale copy scaled down to at most
// analysisWidth pixels wide, so a 4K snapshot costs the same as a small one.
const (
	analysisWidth = 320
	thumbW        = 64
	thumbH        = 48
)

// measures of one snapshot.
type measures struct {
	width, height int
	brightness    float64 // mean luma, percent of full scale
	contrast      float64 // standard deviation of luma, 0..255
	sharpness     float64 // Laplacian variance divided by luma variance: independent of lighting
	hash          uint64  // difference hash of the scene (9x8)
	thumb         []byte  // 64x48 luma, for frozen-picture detection
}

// analyze decodes a snapshot and measures it.
func analyze(b []byte) (measures, error) {
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return measures{}, err
	}
	bounds := img.Bounds()
	m := measures{width: bounds.Dx(), height: bounds.Dy()}
	if m.width == 0 || m.height == 0 {
		return m, errEmpty
	}
	w := min(analysisWidth, m.width)
	h := max(1, m.height*w/m.width)
	gray := scale(img, w, h)

	var sum, sum2 float64
	for _, v := range gray {
		sum += v
		sum2 += v * v
	}
	n := float64(len(gray))
	mean := sum / n
	variance := math.Max(sum2/n-mean*mean, 0)
	m.brightness = mean / 255 * 100
	m.contrast = math.Sqrt(variance)

	// Variance of the 4-neighbour Laplacian: high when edges are crisp.
	var ls, ls2 float64
	var ln float64
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			l := 4*gray[y*w+x] - gray[y*w+x-1] - gray[y*w+x+1] - gray[(y-1)*w+x] - gray[(y+1)*w+x]
			ls += l
			ls2 += l * l
			ln++
		}
	}
	if ln > 0 && variance > 0 {
		lm := ls / ln
		m.sharpness = math.Max(ls2/ln-lm*lm, 0) / variance
	}

	small := scale(img, 9, 8)
	for y := range 8 {
		for x := range 8 {
			m.hash <<= 1
			if small[y*9+x] < small[y*9+x+1] {
				m.hash |= 1
			}
		}
	}
	t := scale(img, thumbW, thumbH)
	m.thumb = make([]byte, len(t))
	for i, v := range t {
		m.thumb[i] = byte(math.Round(v))
	}
	return m, nil
}

// scale returns the luma of img averaged into w x h cells.
func scale(img image.Image, w, h int) []float64 {
	b := img.Bounds()
	out := make([]float64, w*h)
	count := make([]float64, w*h)
	// Sample at most about 4 points per output cell in each direction.
	stepX := max(1, b.Dx()/(w*4))
	stepY := max(1, b.Dy()/(h*4))
	for y := b.Min.Y; y < b.Max.Y; y += stepY {
		cy := (y - b.Min.Y) * h / b.Dy()
		for x := b.Min.X; x < b.Max.X; x += stepX {
			cx := (x - b.Min.X) * w / b.Dx()
			r, g, bb, _ := img.At(x, y).RGBA()
			// ITU-R BT.601 luma, 16-bit channels to 0..255.
			l := (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bb)) / 257
			out[cy*w+cx] += l
			count[cy*w+cx]++
		}
	}
	for i := range out {
		if count[i] > 0 {
			out[i] /= count[i]
		}
	}
	return out
}

// sceneChange is the share of differing bits of two scene hashes, in percent.
func sceneChange(a, b uint64) float64 {
	return float64(bits.OnesCount64(a^b)) / 64 * 100
}

// meanAbsDiff compares two thumbnails, 0..255.
func meanAbsDiff(a, b []byte) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return math.Inf(1)
	}
	var s float64
	for i := range a {
		s += math.Abs(float64(a[i]) - float64(b[i]))
	}
	return s / float64(len(a))
}

type imageError string

func (e imageError) Error() string { return string(e) }

const errEmpty = imageError("the snapshot is empty")

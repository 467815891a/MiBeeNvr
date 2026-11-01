package recorder

import (
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildFFmpegArgsWatermark asserts that the burn-in filter reaches
// FFmpeg as a `-vf` between the input and the encoder options — FFmpeg
// rejects a filter placed after the output options.
func TestBuildFFmpegArgsWatermark(t *testing.T) {
	filter := watermarkFilterGraph(WatermarkSpec{FontFile: `C:\Windows\Fonts\consola.ttf`, EpochSeconds: 1759000000})
	args := buildFFmpegArgs("HD Webcam", 0, true, 1920, 1080, 30, "h264_qsv", filter)
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "-vf ") {
		t.Fatalf("watermark args must carry -vf:\n%s", joined)
	}
	i := indexOf(args, "-i")
	vf := indexOf(args, "-vf")
	codec := indexOf(args, "-c:v")
	if i < 0 || vf < 0 || codec < 0 || vf < i || codec < vf {
		t.Fatalf("-vf must sit between -i and -c:v:\n%s", joined)
	}
	if args[vf+1] != filter {
		t.Fatalf("-vf must carry the filtergraph verbatim")
	}
}

// TestBuildFFmpegArgsNoWatermark asserts that a capture without the burn-in
// keeps the plain pipeline (no empty -vf).
func TestBuildFFmpegArgsNoWatermark(t *testing.T) {
	args := buildFFmpegArgs("HD Webcam", 0, true, 1920, 1080, 30, "", "")
	if indexOf(args, "-vf") >= 0 {
		t.Fatalf("no watermark must not add -vf: %v", args)
	}
}

// TestWatermarkFilterGraph pins the pieces the filter chain depends on:
// the wall-clock re-base, the local-time expansion, the adaptive recolour and
// the escaped font path.
func TestWatermarkFilterGraph(t *testing.T) {
	g := watermarkFilterGraph(WatermarkSpec{FontFile: `C:\Windows\Fonts\consola.ttf`, EpochSeconds: 1759000000})
	for _, want := range []string{
		"setpts=PTS-STARTPTS+1759000000/TB",
		"split=3[wm_base][wm_txt][wm_lum]",
		`fontfile='C\:/Windows/Fonts/consola.ttf'`,
		"text='%{pts\\:localtime}'",
		"fontcolor=white",
		"scale=1:1",
		"geq=",
		"if(gt(r(0\\,0)\\,127)\\,0\\,255)",
		"overlay=x=0:y=0",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("filtergraph missing %q:\n%s", want, g)
		}
	}
	// The stamp belongs in the top-left corner.
	if !strings.Contains(g, ":x=12:y=8") {
		t.Errorf("filtergraph must place the stamp top-left:\n%s", g)
	}
}

// TestEscapeFilterPath asserts the filtergraph escaping of a Windows path.
func TestEscapeFilterPath(t *testing.T) {
	got := escapeFilterPath(`C:\Windows\Fonts\consola.ttf`)
	if want := `C\:/Windows/Fonts/consola.ttf`; got != want {
		t.Fatalf("escapeFilterPath = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "'") {
		t.Fatalf("a quote in the path would break the filtergraph: %q", got)
	}
}

// TestWatermarkFilterGraphRuns is the end-to-end check of the adaptive
// colour: FFmpeg renders the stamp over a dark clip (white glyphs) and over a
// bright clip (black glyphs). It is skipped when no FFmpeg is installed.
func TestWatermarkFilterGraphRuns(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed — skipping the burn-in render test")
	}
	font := ResolveWatermarkFont()
	if font == "" {
		t.Skip("no font file available — skipping the burn-in render test")
	}
	filter := watermarkFilterGraph(WatermarkSpec{FontFile: font, EpochSeconds: 1759000000})
	t.Logf("watermark filtergraph:\n%s", filter)

	dir := t.TempDir()
	// "color=black" for the first second, white afterwards: the stamp has to
	// follow the backdrop frame by frame.
	src := "color=c=black:s=320x240:r=5:d=2,geq=lum='if(gt(T\\,0.5)\\,255\\,0)':cb=128:cr=128"
	for _, tc := range []struct {
		name      string
		seek      string
		wantLight bool // true = the glyphs must be bright (dark backdrop)
	}{
		{name: "dark.png", seek: "0.1", wantLight: true},
		{name: "bright.png", seek: "1.1", wantLight: false},
	} {
		out := filepath.Join(dir, tc.name)
		cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
			"-ss", tc.seek, "-f", "lavfi", "-i", src, "-vf", filter,
			"-frames:v", "1", "-y", out)
		if outBytes, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %s failed: %v\n%s", tc.name, err, outBytes)
		}
		f, err := os.Open(out)
		if err != nil {
			t.Fatalf("open %s: %v", tc.name, err)
		}
		img, err := png.Decode(f)
		f.Close()
		if err != nil {
			t.Fatalf("decode %s: %v", tc.name, err)
		}
		light, dark := countGlyphPixels(img)
		if tc.wantLight && light == 0 {
			t.Errorf("%s: expected bright glyphs over the dark backdrop", tc.name)
		}
		if !tc.wantLight && dark == 0 {
			t.Errorf("%s: expected dark glyphs over the bright backdrop", tc.name)
		}
	}
}

// countGlyphPixels counts the bright and the dark pixels inside the stamp
// area (top-left corner). The backdrop itself is uniform, so a pixel that
// differs from it is a glyph.
func countGlyphPixels(img image.Image) (light, dark int) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Min.Y+60 && y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Min.X+260 && x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			lum := (r + g + bl) / 3 >> 8
			switch {
			case lum > 200:
				light++
			case lum < 40:
				dark++
			}
		}
	}
	return light, dark
}

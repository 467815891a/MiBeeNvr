package recorder

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Time watermark (burn-in) for the UVC capture pipeline.
//
// The stamp is rendered by FFmpeg inside the capture filter chain, so it is
// part of the encoded stream: it ends up in the recorded segments AND in the
// live preview, with no second pass over the footage.
//
// Adaptive colour
// ---------------
// drawtext cannot pick its own colour: `fontcolor` takes a literal colour and
// the drawtext expression language has no access to the pixels it draws on
// (`fontcolor_expr` exists, but FFmpeg parses its value as a colour literal
// instead of evaluating it). The colour is therefore decided by the filter
// chain itself:
//
//	[base][txt][lum]                        split the capture into 3 branches
//	lum → crop(text box) → scale(1:1)       average luminance behind the text
//	txt → drawtext on a transparent copy     the glyph coverage ends up in alpha
//	probe (the 1x1 average) overlaid at 0,0  the average lands in pixel (0,0)
//	geq over the text box                    repaint every pixel black or white
//	                                         by that value, keep the alpha
//	base + recoloured box (overlay at 0,0)   the burned-in watermark
//
// The average is taken over exactly the area the glyphs sit on, so the stamp
// is white over a dark backdrop and black over a bright one — decided per
// frame, not per recording.

// WatermarkSpec describes the burn-in watermark of one capture.
type WatermarkSpec struct {
	// FontFile is the absolute path of the TrueType font used for the text.
	FontFile string
	// EpochSeconds is the wall-clock time the capture starts at. The frame
	// timestamps are re-based onto it so the drawn clock matches the real one.
	EpochSeconds int64
}

// Filter graph fragments.
//
// The font size scales with the frame: a fixed size would be unreadable on a
// 640x480 stick camera and would swallow a 4K frame. drawtext knows the frame
// as w/h, crop knows it as iw/ih — the same values.
const (
	// wmFontSize is the glyph size: 1/30 of the short side, never below 14px.
	wmFontSize = `max(min(w\,h)/30\,14)`

	// wmBoxWidth is the width of the area that carries the stamp. The text
	// ("2025-09-28 03:06:40") is ~10.5 glyph advances wide in a 0.55em mono
	// font, so 11x the size plus slack always covers it.
	wmBoxWidth = `11*max(min(iw\,ih)/30\,14)+40`

	// wmBoxHeight is the height of that area: the glyphs are ~1.2x the size
	// tall.
	wmBoxHeight = `2*max(min(iw\,ih)/30\,14)+24`

	// wmRecolor repaints the text box: every pixel becomes black or white
	// depending on the average luminance the probe pixel carries, while the
	// alpha (the glyph coverage) is kept. The alpha of pixel (0,0) — the
	// probe itself — is cleared so that it stays invisible.
	wmRecolor = `r='if(gt(r(0\,0)\,127)\,0\,255)':g='if(gt(r(0\,0)\,127)\,0\,255)':b='if(gt(r(0\,0)\,127)\,0\,255)':a='if(lt(X\,1)*lt(Y\,1)\,0\,alpha(X\,Y))'`
)

// watermarkFilterGraph builds the FFmpeg `-vf` chain that burns the
// wall-clock time into the top-left corner of the captured frames.
//
// The result is a single filtergraph argument: the caller passes it to
// FFmpeg verbatim (no shell involved), so the only escaping that matters is
// the one FFmpeg's own filter parser expects — commas inside expressions and
// the colon of a Windows drive letter.
func watermarkFilterGraph(spec WatermarkSpec) string {
	font := escapeFilterPath(spec.FontFile)
	parts := []string{
		// Re-base the timestamps onto the wall clock: drawtext renders
		// %{pts:localtime}, which formats the PTS as a Unix timestamp.
		// STARTPTS keeps the result independent of the timestamp base the
		// capture device happens to use.
		//
		// setpts and split share one chain (a comma): a ';' would start a
		// second chain, and a chain without an input label is a second graph
		// input — FFmpeg rejects the graph then.
		"setpts=PTS-STARTPTS+" + strconv.FormatInt(spec.EpochSeconds, 10) + "/TB,split=3[wm_base][wm_txt][wm_lum]",
		"[wm_txt]format=rgba,colorchannelmixer=aa=0,drawtext=fontfile='" + font +
			"':text='%{pts\\:localtime}':fontcolor=white:fontsize=" + wmFontSize +
			":x=12:y=8[wm_text]",
		"[wm_lum]crop=w=min(iw\\," + wmBoxWidth + "):h=min(ih\\," + wmBoxHeight +
			"):x=0:y=0,scale=1:1,format=gray[wm_avg]",
		"[wm_text]crop=w=min(iw\\," + wmBoxWidth + "):h=min(ih\\," + wmBoxHeight +
			"):x=0:y=0[wm_box]",
		"[wm_avg]format=rgba[wm_probe]",
		"[wm_box][wm_probe]overlay=x=0:y=0:format=auto[wm_marked]",
		"[wm_marked]format=rgba,geq=" + wmRecolor + "[wm_ink]",
		"[wm_base][wm_ink]overlay=x=0:y=0:format=auto",
	}
	return strings.Join(parts, ";")
}

// escapeFilterPath prepares a file path for use inside a filtergraph option.
//
// The path is single-quoted, so the quote character itself is the only
// dangerous one; the colon is escaped as well because a Windows drive letter
// would otherwise be read as an option separator.
func escapeFilterPath(p string) string {
	p = filepath.ToSlash(p)
	p = strings.ReplaceAll(p, "'", "_")
	return strings.ReplaceAll(p, ":", `\:`)
}

// ResolveWatermarkFont returns the path of a font file FFmpeg's drawtext can
// render, or "" when none is available.
//
// A missing font must never take the recording down: drawtext aborts FFmpeg
// at startup when it cannot open its fontfile, and the camera would stop
// recording altogether. The caller therefore treats "" as "record without a
// watermark".
func ResolveWatermarkFont() string {
	for _, p := range watermarkFontCandidates() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 {
			return p
		}
	}
	return ""
}

// watermarkFontCandidates lists the fonts to try, best first. A monospaced
// font keeps the timestamp from jittering as the digits change.
func watermarkFontCandidates() []string {
	var out []string
	if root := os.Getenv("SystemRoot"); root != "" {
		out = append(out,
			filepath.Join(root, "Fonts", "consola.ttf"),
			filepath.Join(root, "Fonts", "cour.ttf"),
			filepath.Join(root, "Fonts", "segoeui.ttf"),
			filepath.Join(root, "Fonts", "arial.ttf"),
		)
	}
	if runtime.GOOS == "windows" {
		out = append(out,
			`C:\Windows\Fonts\consola.ttf`,
			`C:\Windows\Fonts\cour.ttf`,
			`C:\Windows\Fonts\segoeui.ttf`,
			`C:\Windows\Fonts\arial.ttf`,
		)
	}
	out = append(out,
		"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationMono-Regular.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
		"/usr/share/fonts/TTF/DejaVuSansMono.ttf",
		"/usr/share/fonts/truetype/noto/NotoSansMono-Regular.ttf",
		"/System/Library/Fonts/Supplemental/Arial.ttf",
		"/Library/Fonts/Arial.ttf",
	)
	return out
}

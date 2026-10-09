package ui

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	visualRootRule = regexp.MustCompile(`:root\s*\{([^}]*)\}`)
	visualVarRule  = regexp.MustCompile(`(--[a-z0-9-]+)\s*:\s*([^;]+);`)
	visualHexColor = regexp.MustCompile(`^#([0-9a-fA-F]{6})$`)
	visualRGBA     = regexp.MustCompile(`^rgba\(\s*([0-9.]+)\s*,\s*([0-9.]+)\s*,\s*([0-9.]+)\s*,\s*([0-9.]+)\s*\)$`)
)

type visualColor struct {
	r, g, b, a float64
}

// TestVisualPaletteContrast checks the actual stylesheet tokens used by both
// production templates. It checks numeric color contrast; Windows rendering,
// forced-colors presentation, keyboard flow, and DPI behavior need separate
// platform evidence.
func TestVisualPaletteContrast(t *testing.T) {
	blocks := visualRootRule.FindAllStringSubmatch(string(CSS()), -1)
	if len(blocks) != 2 {
		t.Fatalf("shared stylesheet has %d palette blocks, want dark and light roles", len(blocks))
	}

	for index, name := range []string{"dark", "light"} {
		tokens := parseVisualTokens(t, blocks[index][1])
		t.Run(name, func(t *testing.T) {
			backgrounds := []string{"bg", "surface", "raised", "inset"}
			textRoles := []string{"text", "text-2", "text-3", "accent"}
			for _, foregroundName := range textRoles {
				foreground := visualTokenColor(t, tokens, foregroundName)
				for _, backgroundName := range backgrounds {
					background := visualTokenColor(t, tokens, backgroundName)
					assertVisualContrast(t, name+" "+foregroundName+" on "+backgroundName, foreground, background, 4.5)
				}
			}

			for _, state := range []string{"ok", "warn", "bad", "idle"} {
				foreground := visualTokenColor(t, tokens, state)
				soft := visualTokenColor(t, tokens, state+"-soft")
				for _, backgroundName := range []string{"bg", "surface", "raised"} {
					background := visualTokenColor(t, tokens, backgroundName)
					assertVisualContrast(t, name+" "+state+" on "+backgroundName, foreground, background, 4.5)
					assertVisualContrast(t, name+" "+state+" on "+state+"-soft over "+backgroundName, foreground, compositeVisualColor(soft, background), 4.5)
				}
			}

			ink := visualTokenColor(t, tokens, "accent-ink")
			accent := visualTokenColor(t, tokens, "accent")
			assertVisualContrast(t, name+" accent ink on accent", ink, accent, 4.5)

			controlBorder := visualTokenColor(t, tokens, "line-2")
			for _, backgroundName := range []string{"inset", "raised"} {
				assertVisualContrast(t, name+" control border on "+backgroundName, controlBorder, visualTokenColor(t, tokens, backgroundName), 3)
			}
		})
	}
}

func parseVisualTokens(t *testing.T, block string) map[string]string {
	t.Helper()
	tokens := make(map[string]string)
	for _, match := range visualVarRule.FindAllStringSubmatch(block, -1) {
		tokens[strings.TrimPrefix(match[1], "--")] = strings.TrimSpace(match[2])
	}
	return tokens
}

func visualTokenColor(t *testing.T, tokens map[string]string, name string) visualColor {
	t.Helper()
	value, ok := tokens[name]
	if !ok {
		t.Fatalf("palette is missing --%s", name)
	}
	if match := visualHexColor.FindStringSubmatch(value); match != nil {
		parsed, err := strconv.ParseUint(match[1], 16, 32)
		if err != nil {
			t.Fatalf("parse --%s color %q: %v", name, value, err)
		}
		return visualColor{
			r: float64(parsed >> 16),
			g: float64((parsed >> 8) & 0xff),
			b: float64(parsed & 0xff),
			a: 1,
		}
	}
	if match := visualRGBA.FindStringSubmatch(value); match != nil {
		parts := make([]float64, 4)
		for index, part := range match[1:] {
			parsed, err := strconv.ParseFloat(part, 64)
			if err != nil {
				t.Fatalf("parse --%s color %q: %v", name, value, err)
			}
			parts[index] = parsed
		}
		return visualColor{r: parts[0], g: parts[1], b: parts[2], a: parts[3]}
	}
	t.Fatalf("palette --%s has unsupported color %q", name, value)
	return visualColor{}
}

func compositeVisualColor(foreground, background visualColor) visualColor {
	return visualColor{
		r: foreground.r*foreground.a + background.r*(1-foreground.a),
		g: foreground.g*foreground.a + background.g*(1-foreground.a),
		b: foreground.b*foreground.a + background.b*(1-foreground.a),
		a: 1,
	}
}

func assertVisualContrast(t *testing.T, name string, foreground, background visualColor, minimum float64) {
	t.Helper()
	first, second := visualLuminance(foreground), visualLuminance(background)
	if first < second {
		first, second = second, first
	}
	ratio := (first + .05) / (second + .05)
	if ratio < minimum {
		t.Errorf("%s contrast is %.2f:1, want at least %.1f:1", name, ratio, minimum)
	}
}

func visualLuminance(color visualColor) float64 {
	linear := func(channel float64) float64 {
		channel /= 255
		if channel <= .04045 {
			return channel / 12.92
		}
		return math.Pow((channel+.055)/1.055, 2.4)
	}
	return .2126*linear(color.r) + .7152*linear(color.g) + .0722*linear(color.b)
}

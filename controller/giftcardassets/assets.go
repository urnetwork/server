// Package giftcardassets holds the files a gift card page loads: its fonts and
// each season's scene. It is its own package so that only the taskworker,
// which prints the cards, links these bytes (about 19 MB, most of it the
// Chinese face); the taskworker installs them with
// controller.InstallGiftCardAssets. Nothing here is executed.
//
// The fonts are open-licensed (SIL Open Font License 1.1, each family's
// license beside it as <Family>-OFL.txt), unmodified, and come from Google
// Fonts' repository, https://github.com/google/fonts, at commit
// 29b4ec6df2522a8aee279414b78a07910b065874, renamed without their axis
// suffix. Each matched its git blob there; the SHA-256 here are of the files
// as committed:
//
//	fonts/Caveat.ttf           ofl/caveat/Caveat[wght].ttf                 0bdb6b660482d31531b3945849fba5916b3ef8695da7024a9e6b9ee3c4157988  the note and signature (Latin, Cyrillic)
//	fonts/Geist.ttf            ofl/geist/Geist[wght].ttf                   73894e0448cae90a92b6c2f8732b7bb9acb7b94c418bff559dad4a18e1de9659  text (Latin, Cyrillic)
//	fonts/GeistMono.ttf        ofl/geistmono/GeistMono[wght].ttf           d00e590b8eb3a59acc329b2d044fd143ae935090b7da33199ebee27cc7de8196  the code
//	fonts/LeagueGothic.ttf     ofl/leaguegothic/LeagueGothic[wdth].ttf     3b0e998c9a0034222394ffecdd383e6948259ad037b95555b139a217629ce1d0  headlines (Latin only)
//	fonts/NotoSansSC.ttf       ofl/notosanssc/NotoSansSC[wght].ttf         a3041811a78c361b1de50f953c805e0244951c21c5bd412f7232ef0d899af0da  Chinese
//	fonts/NotoNaskhArabic.ttf  ofl/notonaskharabic/NotoNaskhArabic[wght].ttf 67b5a525a661b607971fbd3f96a81b89d3a768e74534fca84f18ac97e6fab72f  Arabic
//
// League Gothic stands in for the site's commercial display face (ABC Gravity
// Extra Condensed), as in the mockup. Its glyphs are Latin only, so a Russian,
// Chinese or Arabic card sets its headline in Geist, Noto Sans SC or Noto Naskh
// Arabic. Every face is the full font: the note is the buyer's own text, in any
// of these scripts.
//
// scenes/<season>.png is a lossless still of the season's 3D scene, the
// render the site ships as WebP (see scenes/SOURCES.txt). A season without one
// prints the mockup's night sky alone.
package giftcardassets

import (
	"embed"
)

// The fonts and scenes, at fonts/ and scenes/.
//
//go:embed fonts scenes
var FS embed.FS

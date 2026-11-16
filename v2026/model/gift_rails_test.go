package model

import (
	"testing"
)

// The table the site mirrors (GIFTOFPRIVACY.md §10.2): what an email client
// would link is a link, and ordinary prose in the site's languages is not.
var giftTextHasLinkCases = []struct {
	text string
	link bool
}{
	// a URL scheme
	{"visit https://example.com", true},
	{"HTTP://EXAMPLE.COM", true},
	{"ftp://files.example.org/x", true},
	{"see foo://bar", true},
	{"x-custom+v2.app://open", true},
	// mailto
	{"mailto:sam@example.com", true},
	{"write MAILTO:sam", true},
	// www.
	{"www.example.com", true},
	{"go to WWW.Example", true},
	{"(www.example.co.uk)", true},
	{"-www.example", true},
	// a domain with a common TLD
	{"example.com", true},
	{"Example.COM.", true},
	{"my site is example.org!", true},
	{"hi! example.net, bye", true},
	{"bit.ly", true},
	{"bit.ly/abc123", true},
	{"ow.ly/x", true},
	{"goo.gl/xyz", true},
	{"t.co/xyz", true},
	{"discord.gg/abc", true},
	{"example.co.uk", true},
	{"example.com.au", true},
	{"sub.domain.example.io", true},
	{"a.b.c.example.net", true},
	{"example.com:8080", true},
	{"example.com- and more", true},
	{"free-gifts.xyz", true},
	{"xn--80ak6aa92e.com", true},
	{"例子.com", true},
	{"مرحبا.com", true},
	{"sam@example.com", true},
	{"...example.com", true},
	// a word-like TLD before "/" or after "@"
	{"t.me/joinchat", true},
	{"wa.me/15551234567", true},
	{"amzn.to/3xYz", true},
	{"youtu.be/dQw4w9WgXcQ", true},
	{"lnkd.in/abc", true},
	{"münchen.de/rathaus", true},
	{"sale.top/now", true},
	{"write to sam.smith@web.de", true},
	{"ana@correo.es", true},

	// prose
	{"Merry Christmas!", false},
	{"Happy holidays 🎄✨", false},
	{"Love, Mom & Dad", false},
	{"3.5 stars", false},
	{"price 23.99 usd", false},
	{"Version 2.0.1", false},
	{"192.168.0.1", false},
	{"e.g. this, i.e. that", false},
	{"U.S.A.", false},
	{"the U.S. and the U.K.", false},
	{"10:30 a.m.", false},
	{"See you at 5.30pm", false},
	{"J.R.R. Tolkien", false},
	{"Ph.D.", false},
	{"Dr. Smith", false},
	{"St. Nicholas", false},
	{"Mr.T", false},
	{"Ann.Marie", false},
	{"Node.js and Vue.js", false},
	{"Love,Grandma.xo", false},
	{"awww so cute", false},
	{"www.", false},
	{"wow ://", false},
	{"smile :/", false},
	{"C:/Users", false},
	{"@sam", false},
	{"a..com", false},
	{"example dot com", false},
	{"example .com", false},
	{"example. com", false},
	{"com.example", false},
	{"example.com-and", false},
	{"rabbit.lyrics", false},
	{"Привет. С Новым годом", false},
	{"新年快乐。", false},
	// a word-like TLD in a sentence that lost its space
	{"Love you.So much", false},
	{"Miss you.Me too", false},
	{"Thank you.It means a lot", false},
	{"Te quiero mucho.Es tuyo", false},
	{"Feliz navidad.Ve a verla", false},
	{"Frohe Weihnachten.Dein Opa", false},
	{"Viel Spaß.Im neuen Jahr", false},
	{"Have a great holiday.Live long", false},
	{"Logged.in", false},
	{"t.me", false},
}

func TestGiftTextHasLink(t *testing.T) {
	for _, test := range giftTextHasLinkCases {
		if got := GiftTextHasLink(test.text); got != test.link {
			t.Errorf("GiftTextHasLink(%q) = %t, want %t", test.text, got, test.link)
		}
	}
}

// Only ASCII A-Z is lowered: a TLD typed in capitals is still found, and no
// other letter is folded into one.
func TestGiftTextHasLinkFoldsOnlyAscii(t *testing.T) {
	if got := giftLowerAscii("ÉXAMPLE.COM İ"); got != "Éxample.com İ" {
		t.Fatalf("lowered %q", got)
	}
}

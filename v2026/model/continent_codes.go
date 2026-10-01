package model

import "strings"

// ISO 3166-1 alpha-2 country code -> Route 53 continent code (connect/
// EXTENDER.md C5).
//
// The geo dns sets are one per continent, and an extender is placed in a set
// by the country its activating address geolocated to. Route 53 knows only
// seven continents -- AF, AN, AS, EU, NA, OC, SA -- so this table is the whole
// vocabulary the publisher has.
//
// Assignments follow the usual geolocation database convention rather than the
// UN M49 geoscheme, because that is what decides the continent of the client
// asking: Cyprus and Russia are EU, Turkey and the Transcaucasus are AS, the
// Caribbean is NA, and the territories around Antarctica are AN. Where a
// mapping disagrees with Route 53's own view of a querying country the effect
// is locality, not reachability -- the client is answered from another
// continent's set, and the default set covers everything either way.
//
// The table is complete for every assigned alpha-2 code, which the tests pin
// against isoCountryNames, plus XK for Kosovo, which is user-assigned but is
// what the ip databases report. A code that is not here has no continent and
// its extenders appear in the global pool only.
var countryContinentCodes = map[string]string{
	// Africa (AF), 58 codes
	"ao": "AF", "bf": "AF", "bi": "AF", "bj": "AF", "bw": "AF", "cd": "AF", "cf": "AF", "cg": "AF",
	"ci": "AF", "cm": "AF", "cv": "AF", "dj": "AF", "dz": "AF", "eg": "AF", "eh": "AF", "er": "AF",
	"et": "AF", "ga": "AF", "gh": "AF", "gm": "AF", "gn": "AF", "gq": "AF", "gw": "AF", "ke": "AF",
	"km": "AF", "ls": "AF", "lr": "AF", "ly": "AF", "ma": "AF", "mg": "AF", "ml": "AF", "mr": "AF",
	"mu": "AF", "mw": "AF", "mz": "AF", "na": "AF", "ne": "AF", "ng": "AF", "re": "AF", "rw": "AF",
	"sc": "AF", "sd": "AF", "sh": "AF", "sl": "AF", "sn": "AF", "so": "AF", "ss": "AF", "st": "AF",
	"sz": "AF", "td": "AF", "tg": "AF", "tn": "AF", "tz": "AF", "ug": "AF", "yt": "AF", "za": "AF",
	"zm": "AF", "zw": "AF",
	// Antarctica (AN), 5 codes
	"aq": "AN", "bv": "AN", "gs": "AN", "hm": "AN", "tf": "AN",
	// Asia (AS), 53 codes
	"ae": "AS", "af": "AS", "am": "AS", "az": "AS", "bd": "AS", "bh": "AS", "bn": "AS", "bt": "AS",
	"cc": "AS", "cn": "AS", "cx": "AS", "ge": "AS", "hk": "AS", "id": "AS", "il": "AS", "in": "AS",
	"io": "AS", "iq": "AS", "ir": "AS", "jo": "AS", "jp": "AS", "kg": "AS", "kh": "AS", "kp": "AS",
	"kr": "AS", "kw": "AS", "kz": "AS", "la": "AS", "lb": "AS", "lk": "AS", "mm": "AS", "mn": "AS",
	"mo": "AS", "mv": "AS", "my": "AS", "np": "AS", "om": "AS", "ph": "AS", "pk": "AS", "ps": "AS",
	"qa": "AS", "sa": "AS", "sg": "AS", "sy": "AS", "th": "AS", "tj": "AS", "tl": "AS", "tm": "AS",
	"tr": "AS", "tw": "AS", "uz": "AS", "vn": "AS", "ye": "AS",
	// Europe (EU), 53 codes
	"ad": "EU", "al": "EU", "at": "EU", "ax": "EU", "ba": "EU", "be": "EU", "bg": "EU", "by": "EU",
	"ch": "EU", "cy": "EU", "cz": "EU", "de": "EU", "dk": "EU", "ee": "EU", "es": "EU", "fi": "EU",
	"fo": "EU", "fr": "EU", "gb": "EU", "gg": "EU", "gi": "EU", "gr": "EU", "hr": "EU", "hu": "EU",
	"ie": "EU", "im": "EU", "is": "EU", "it": "EU", "je": "EU", "li": "EU", "lt": "EU", "lu": "EU",
	"lv": "EU", "mc": "EU", "md": "EU", "me": "EU", "mk": "EU", "mt": "EU", "nl": "EU", "no": "EU",
	"pl": "EU", "pt": "EU", "ro": "EU", "rs": "EU", "ru": "EU", "se": "EU", "si": "EU", "sj": "EU",
	"sk": "EU", "sm": "EU", "ua": "EU", "va": "EU", "xk": "EU",
	// North America (NA), 41 codes
	"ag": "NA", "ai": "NA", "aw": "NA", "bb": "NA", "bl": "NA", "bm": "NA", "bq": "NA", "bs": "NA",
	"bz": "NA", "ca": "NA", "cr": "NA", "cu": "NA", "cw": "NA", "dm": "NA", "do": "NA", "gd": "NA",
	"gl": "NA", "gp": "NA", "gt": "NA", "hn": "NA", "ht": "NA", "jm": "NA", "kn": "NA", "ky": "NA",
	"lc": "NA", "mf": "NA", "mq": "NA", "ms": "NA", "mx": "NA", "ni": "NA", "pa": "NA", "pm": "NA",
	"pr": "NA", "sv": "NA", "sx": "NA", "tc": "NA", "tt": "NA", "us": "NA", "vc": "NA", "vg": "NA",
	"vi": "NA",
	// Oceania (OC), 26 codes
	"as": "OC", "au": "OC", "ck": "OC", "fj": "OC", "fm": "OC", "gu": "OC", "ki": "OC", "mh": "OC",
	"mp": "OC", "nc": "OC", "nf": "OC", "nr": "OC", "nu": "OC", "nz": "OC", "pf": "OC", "pg": "OC",
	"pn": "OC", "pw": "OC", "sb": "OC", "tk": "OC", "to": "OC", "tv": "OC", "um": "OC", "vu": "OC",
	"wf": "OC", "ws": "OC",
	// South America (SA), 14 codes
	"ar": "SA", "bo": "SA", "br": "SA", "cl": "SA", "co": "SA", "ec": "SA", "fk": "SA", "gf": "SA",
	"gy": "SA", "pe": "SA", "py": "SA", "sr": "SA", "uy": "SA", "ve": "SA",
}

// ContinentCodeForCountry maps an ISO 3166-1 alpha-2 country code, in any
// case, to its Route 53 continent code.
//
// An empty or unknown code returns "", which is not a continent and never a
// guess: an extender the operator cannot place still serves the global pool,
// while a wrong placement would advertise it as local to a continent it is
// nowhere near.
func ContinentCodeForCountry(countryCode string) string {
	return countryContinentCodes[strings.ToLower(strings.TrimSpace(countryCode))]
}

// The continents a geo dns set can be published for, in a stable order so one
// tick's change batch reads the same as the next.
var ContinentCodes = []string{"AF", "AN", "AS", "EU", "NA", "OC", "SA"}

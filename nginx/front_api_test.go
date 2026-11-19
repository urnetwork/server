package nginx

// Tests of the API skeleton's own code. The front's behavior is tested by its
// implementation.

import (
	"fmt"
	"testing"
)

// Each class has its own short name, and the zero value is unknown, so an
// unset class never reads as a served one.
func TestNameClassString(t *testing.T) {
	var zeroNameClass NameClass
	if zeroNameClass != NameClassUnknown {
		t.Fatalf("the zero NameClass is %d, want NameClassUnknown", zeroNameClass)
	}
	cases := []struct {
		nameClass NameClass
		want      string
	}{
		{nameClass: NameClassUnknown, want: "unknown"},
		{nameClass: NameClassLb, want: "lb"},
		{nameClass: NameClassConnect, want: "connect"},
		{nameClass: NameClassApi, want: "api"},
		{nameClass: NameClassForwarded, want: "forwarded"},
		{nameClass: NameClassRedirect, want: "redirect"},
		{nameClass: NameClassRedirect + 1, want: "unknown"},
		{nameClass: NameClass(-1), want: "unknown"},
	}
	for _, c := range cases {
		if got := fmt.Sprintf("%s", c.nameClass); got != c.want {
			t.Errorf("NameClass(%d) = %q, want %q", int(c.nameClass), got, c.want)
		}
	}
}

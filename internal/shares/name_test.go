package shares

import "testing"

func TestValidName(t *testing.T) {
	cases := []struct {
		goos, name string
		want       bool
	}{
		{"windows", "a:b", false}, {"windows", "a?b", false}, {"windows", "CON", false},
		{"windows", "nul.txt", false}, {"windows", "a.", false}, {"windows", "a ", false},
		{"windows", "com1", false}, {"windows", "a\\b", false}, {"windows", "a\x01b", false},
		{"windows", "CONIN$", false}, {"windows", "conout$", false}, {"windows", "COM\u00b9", false},
		{"windows", "lpt\u00b3.txt", false}, {"windows", "COM0", true}, {"windows", "COM10", true},
		{"windows", "a.txt", true}, {"windows", "한글.txt", true}, {"windows", "console", true},
		{"linux", "a:b", true}, {"linux", "CON", true}, {"linux", "a.", true},
		{"darwin", "a:b", true},
		{"linux", "", false}, {"linux", "a\x00b", false}, {"linux", "a/b", false},
		{"windows", "", false}, {"windows", "a\x00b", false}, {"windows", "a/b", false},
		{"darwin", "", false}, {"darwin", "a\x00b", false}, {"darwin", "a/b", false},
		{"linux", ".", false}, {"linux", "..", false},
	}
	for _, c := range cases {
		if got := ValidName(c.goos, c.name); got != c.want {
			t.Errorf("ValidName(%s,%q)=%v want %v", c.goos, c.name, got, c.want)
		}
	}
}

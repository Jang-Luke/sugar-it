package winout

import "testing"

func TestDecodeKeepsUTF8(t *testing.T) {
	for _, s := range []string{"", "plain", "한글 출력"} {
		if got := Decode([]byte(s)); got != s {
			t.Fatalf("Decode(%q) = %q", s, got)
		}
	}
}

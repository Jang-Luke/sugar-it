package mesh

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInviteRoundTrip(t *testing.T) {
	in := Invite{ID: "abc", Name: "nas", Addrs: []string{"192.168.0.5:7443"}, Token: "tok", Expires: time.Unix(1700000000, 0).UTC()}
	s := in.Encode()
	if !strings.HasPrefix(s, "sugar-it1:") {
		t.Fatal(s)
	}
	out, err := DecodeInvite(s)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("%+v != %+v", in, out)
	}
}

func TestDecodeInviteRejectsGarbage(t *testing.T) {
	enc := func(js string) string { return invitePrefix + base64.RawURLEncoding.EncodeToString([]byte(js)) }
	for name, s := range map[string]string{
		"empty":      "",
		"no prefix":  base64.RawURLEncoding.EncodeToString([]byte(`{}`)),
		"bad base64": invitePrefix + "!!!",
		"bad json":   enc(`nope`),
		"no id":      enc(`{"name":"n","addrs":["a:1"],"token":"t","expires":"2030-01-01T00:00:00Z"}`),
		"no name":    enc(`{"id":"i","addrs":["a:1"],"token":"t","expires":"2030-01-01T00:00:00Z"}`),
		"no addrs":   enc(`{"id":"i","name":"n","token":"t","expires":"2030-01-01T00:00:00Z"}`),
		"no token":   enc(`{"id":"i","name":"n","addrs":["a:1"],"expires":"2030-01-01T00:00:00Z"}`),
		"no expires": enc(`{"id":"i","name":"n","addrs":["a:1"],"token":"t"}`),
	} {
		if _, err := DecodeInvite(s); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestTokenOneTime(t *testing.T) {
	tk := NewTokens(time.Now)
	tok := tk.Issue(time.Minute)
	if len(tok) < 22 {
		t.Fatalf("token too short: %q", tok)
	}
	if !tk.Consume(tok) {
		t.Fatal("first consume failed")
	}
	if tk.Consume(tok) {
		t.Fatal("second consume succeeded")
	}
	if tk.Consume("bogus") {
		t.Fatal("bogus accepted")
	}
}

func TestTokenExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	tk := NewTokens(func() time.Time { return now })
	tok := tk.Issue(10 * time.Minute)
	now = now.Add(10*time.Minute + time.Second)
	if tk.Consume(tok) {
		t.Fatal("expired token accepted")
	}
}

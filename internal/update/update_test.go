package update

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseTag(t *testing.T) {
	for tag, want := range map[string]bool{
		"v1.2.3": true, "v0.0.0": true, "v10.20.30": true,
		"1.2.3": false, "v1.2": false, "v1.2.3.4": false, "v1.2.3-rc1": false, "v1.2.3+b": false,
		"V1.2.3": false, " v1.2.3": false, "v1.2.3\n": false, "": false, "vx.y.z": false, "dev": false,
		"v1.2.99999999999999999999": false,
	} {
		if _, ok := ParseTag(tag); ok != want {
			t.Errorf("ParseTag(%q) = %v, want %v", tag, ok, want)
		}
	}
}

func TestNewerIsStrictAndNumeric(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v1.2.4", "v1.2.3", true},
		{"v1.3.0", "v1.2.9", true},
		{"v2.0.0", "v1.9.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.2", "v1.2.3", false},
		{"v0.9.0", "v0.10.0", false},
	} {
		a, _ := ParseTag(c.a)
		b, _ := ParseTag(c.b)
		if got := a.Newer(b); got != c.want {
			t.Errorf("%s.Newer(%s) = %v", c.a, c.b, got)
		}
	}
}

func TestParseVersionIsThePlainForm(t *testing.T) {
	if v, ok := ParseVersion("9.9.9"); !ok || v != (Version{9, 9, 9}) {
		t.Errorf("ParseVersion(9.9.9) = %v %v", v, ok)
	}
	for _, s := range []string{"dev", "", "v1.2.3", "1.2", "1.2.3-rc1"} {
		if _, ok := ParseVersion(s); ok {
			t.Errorf("ParseVersion(%q) accepted", s)
		}
	}
}

func TestIsReleaseURL(t *testing.T) {
	for u, want := range map[string]bool{
		"https://github.com/Jang-Luke/sugar-it/releases/":                      true,
		"https://github.com/Jang-Luke/sugar-it/releases/tag/v1.2.3":            true,
		"https://github.com/Jang-Luke/sugar-it/releases/latest":                true,
		"https://github.com/Jang-Luke/sugar-it/releases":                       false,
		"http://github.com/Jang-Luke/sugar-it/releases/tag/v1.2.3":             false,
		"https://github.com/Jang-Luke/sugar-itx/releases/":                     false,
		"https://github.com.evil.example/Jang-Luke/sugar-it/releases/":         false,
		"https://evil.example/https://github.com/Jang-Luke/sugar-it/releases/": false,
		"https://github.com/Jang-Luke/sugar-it/releases/../../../evil":         false,
		"https://github.com/Jang-Luke/sugar-it/releases/%2e%2e/x":              false,
		"https://github.com/Jang-Luke/sugar-it/releases/a\\..\\b":              false,
		"https://github.com/Jang-Luke/sugar-it/releases/x?u=y":                 false,
		"https://github.com/Jang-Luke/sugar-it/releases/x#y":                   false,
		"https://github.com/Jang-Luke/sugar-it/releases/x\ny":                  false,
		"": false,
	} {
		if got := IsReleaseURL(u); got != want {
			t.Errorf("IsReleaseURL(%q) = %v", u, got)
		}
	}
}

type apiAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

func dl(tag, name string) apiAsset {
	return apiAsset{Name: name, Size: 10, URL: DownloadURL + tag + "/" + name}
}

// apiServer answers the releases/latest path only, and records what it saw.
func apiServer(t *testing.T, status int, tag string, assets []apiAsset) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var hits atomic.Int32
	var ua atomic.Value
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		ua.Store(r.Header.Get("User-Agent") + "|" + r.Header.Get("Authorization"))
		if r.URL.Path != "/repos/Jang-Luke/sugar-it/releases/latest" {
			t.Errorf("path %s", r.URL.Path)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			io.WriteString(w, `{"message":"<script>alert(1)</script>"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "body": "<img src=x onerror=alert(1)>", "assets": assets})
	}))
	t.Cleanup(s.Close)
	return s, &hits, &ua
}

func TestCheckFindsANewerRelease(t *testing.T) {
	s, _, ua := apiServer(t, 200, "v1.2.4", []apiAsset{dl("v1.2.4", InstallerAsset), dl("v1.2.4", ChecksumAsset), dl("v1.2.4", "other.zip")})
	c := Checker{BaseURL: s.URL, HTTP: s.Client(), Current: "1.2.3"}
	rel, newer, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !newer || rel.Tag != "v1.2.4" || !rel.Installable() {
		t.Errorf("rel = %+v, newer = %v", rel, newer)
	}
	if got := ua.Load(); got != "sugar-it-desktop/1.2.3|" {
		t.Errorf("User-Agent and Authorization = %q", got)
	}
	if rel.PageURL() != ReleasesURL+"tag/v1.2.4" || !IsReleaseURL(rel.PageURL()) {
		t.Errorf("PageURL = %q", rel.PageURL())
	}
}

func TestCheckEqualAndOlderAreNotNewer(t *testing.T) {
	for _, tag := range []string{"v1.2.3", "v1.2.2", "v0.9.9"} {
		s, _, _ := apiServer(t, 200, tag, nil)
		_, newer, err := Checker{BaseURL: s.URL, HTTP: s.Client(), Current: "1.2.3"}.Check(context.Background())
		if err != nil || newer {
			t.Errorf("%s: newer = %v, err = %v", tag, newer, err)
		}
	}
}

func TestCheckRefusesABadTag(t *testing.T) {
	for _, tag := range []string{"v9.9.9-rc1", "latest", "9.9.9", ""} {
		s, _, _ := apiServer(t, 200, tag, nil)
		_, newer, err := Checker{BaseURL: s.URL, HTTP: s.Client(), Current: "1.2.3"}.Check(context.Background())
		if newer || !errors.Is(err, ErrBadRelease) {
			t.Errorf("%q: newer = %v, err = %v", tag, newer, err)
		}
	}
}

func TestCheckFailuresAreErrorsWithoutTheBody(t *testing.T) {
	for _, status := range []int{403, 404, 429, 500} {
		s, hits, _ := apiServer(t, status, "", nil)
		_, newer, err := Checker{BaseURL: s.URL, HTTP: s.Client(), Current: "1.2.3"}.Check(context.Background())
		if newer || err == nil || strings.Contains(err.Error(), "script") {
			t.Errorf("%d: newer = %v, err = %v", status, newer, err)
		}
		if status == 404 && !errors.Is(err, ErrNoRelease) {
			t.Errorf("404: %v", err)
		}
		if hits.Load() != 1 {
			t.Errorf("%d: %d requests, want no retry", status, hits.Load())
		}
	}
}

func TestDevBuildMakesNoRequest(t *testing.T) {
	s, hits, _ := apiServer(t, 200, "v1.2.4", nil)
	for _, v := range []string{"dev", "", "1.2.3-rc1"} {
		c := Checker{BaseURL: s.URL, HTTP: s.Client(), Current: v}
		if c.Enabled() {
			t.Errorf("%q: enabled", v)
		}
		if _, _, err := c.Check(context.Background()); !errors.Is(err, ErrDevBuild) {
			t.Errorf("%q: %v", v, err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests", hits.Load())
	}
}

func TestCheckTimesOut(t *testing.T) {
	done := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-done }))
	defer s.Close()
	defer close(done)
	c := Checker{BaseURL: s.URL, HTTP: s.Client(), Current: "1.2.3", Timeout: 50 * time.Millisecond}
	start := time.Now()
	if _, _, err := c.Check(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the timeout did not apply")
	}
}

func TestDefaultTimeoutIsFifteenSeconds(t *testing.T) {
	if CheckTimeout != 15*time.Second || CheckInterval != 6*time.Hour {
		t.Errorf("CheckTimeout %v, CheckInterval %v", CheckTimeout, CheckInterval)
	}
}

// Only exact URLs under the release's own download path count as its assets.
func TestAssetsOutsideTheReleaseDoNotCount(t *testing.T) {
	bad := []string{
		"https://evil.example/" + InstallerAsset,
		"http://github.com/Jang-Luke/sugar-it/releases/download/v1.2.4/" + InstallerAsset,
		"https://github.com/Jang-Luke/other/releases/download/v1.2.4/" + InstallerAsset,
		DownloadURL + "v1.2.3/" + InstallerAsset, // another release's
		DownloadURL + "v1.2.4/../v0.1.0/" + InstallerAsset,
	}
	for _, u := range bad {
		s, _, _ := apiServer(t, 200, "v1.2.4", []apiAsset{{Name: InstallerAsset, Size: 10, URL: u}, dl("v1.2.4", ChecksumAsset)})
		rel, _, err := Checker{BaseURL: s.URL, HTTP: s.Client(), Current: "1.2.3"}.Check(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if rel.Installable() {
			t.Errorf("%s counted as the installer", u)
		}
	}
}

func TestInstallableNeedsBothAssets(t *testing.T) {
	for _, assets := range [][]apiAsset{nil, {dl("v1.2.4", InstallerAsset)}, {dl("v1.2.4", ChecksumAsset)}} {
		s, _, _ := apiServer(t, 200, "v1.2.4", assets)
		rel, newer, err := Checker{BaseURL: s.URL, HTTP: s.Client(), Current: "1.2.3"}.Check(context.Background())
		if err != nil || !newer || rel.Installable() {
			t.Errorf("%v: %+v %v %v", assets, rel, newer, err)
		}
	}
}

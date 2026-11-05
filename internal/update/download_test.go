package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// toServer sends every request to s, whatever its host, so the real URLs are used.
type toServer struct{ url *url.URL }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = t.url.Scheme, t.url.Host
	return http.DefaultTransport.RoundTrip(r)
}

func serverClient(t *testing.T, h http.Handler) *http.Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: toServer{u}}
}

func testRelease() Release {
	return Release{
		Tag:       "v1.2.4",
		Installer: &Asset{URL: DownloadURL + "v1.2.4/" + InstallerAsset, Size: 1000},
		Checksum:  &Asset{URL: DownloadURL + "v1.2.4/" + ChecksumAsset, Size: 100},
	}
}

func sumLine(data []byte, name string) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:]) + "  " + name + "\n"
}

func assetServer(installer []byte, checksum string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case InstallerAsset:
			w.Write(installer)
		case ChecksumAsset:
			if checksum == "" {
				w.WriteHeader(404)
				return
			}
			io.WriteString(w, checksum)
		default:
			w.WriteHeader(404)
		}
	})
}

func emptyDir(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("left in the directory: %v", ents)
	}
}

func TestDownloadVerifiesAndReportsProgress(t *testing.T) {
	data := bytes.Repeat([]byte("sugar"), 200_000)
	c := serverClient(t, assetServer(data, sumLine(data, InstallerAsset)))
	dir := t.TempDir()
	var last atomic.Int64
	rel := testRelease()
	rel.Installer.Size = int64(len(data))
	got0, err := Downloader{HTTP: c}.Installer(context.Background(), rel, dir, func(done, total int64) {
		last.Store(done)
		if total != int64(len(data)) {
			t.Errorf("total %d", total)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	path := got0.Path
	if filepath.Dir(path) != dir || filepath.Base(path) != InstallerAsset {
		t.Errorf("path %s", path)
	}
	want := sha256.Sum256(data)
	if got0.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("SHA256 %s", got0.SHA256)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("content differs, err %v", err)
	}
	if last.Load() != int64(len(data)) {
		t.Errorf("progress ended at %d", last.Load())
	}
}

func TestChecksumFileForms(t *testing.T) {
	data := []byte("installer")
	h := sha256.Sum256(data)
	hexs := hex.EncodeToString(h[:])
	for name, line := range map[string]string{
		"two spaces": hexs + "  " + InstallerAsset + "\n",
		"binary":     hexs + " *" + InstallerAsset + "\n",
		"CRLF":       hexs + "  " + InstallerAsset + "\r\n",
		"no newline": hexs + "  " + InstallerAsset,
	} {
		c := serverClient(t, assetServer(data, line))
		if _, err := (Downloader{HTTP: c}).Installer(context.Background(), testRelease(), t.TempDir(), nil); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBadChecksumRefusesAndDeletes(t *testing.T) {
	data := []byte("installer")
	for name, tc := range map[string]struct {
		line string
		want error
	}{
		"other content":  {sumLine([]byte("tampered"), InstallerAsset), ErrChecksumMismatch},
		"missing":        {"", ErrNoChecksum},
		"other name":     {sumLine(data, "other.exe"), ErrNoChecksum},
		"a path in name": {sumLine(data, "dir/"+InstallerAsset), ErrNoChecksum},
		"garbage":        {"not a checksum", ErrNoChecksum},
		"short hash":     {"abcd  " + InstallerAsset, ErrNoChecksum},
		"two lines":      {sumLine(data, InstallerAsset) + sumLine(data, InstallerAsset), ErrNoChecksum},
	} {
		c := serverClient(t, assetServer(data, tc.line))
		dir := t.TempDir()
		_, err := Downloader{HTTP: c}.Installer(context.Background(), testRelease(), dir, nil)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
		emptyDir(t, dir)
	}
}

func TestReleaseWithoutTheAssetsIsRefused(t *testing.T) {
	var hits atomic.Int32
	c := serverClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	noSum := testRelease()
	noSum.Checksum = nil
	if _, err := (Downloader{HTTP: c}).Installer(context.Background(), noSum, t.TempDir(), nil); !errors.Is(err, ErrNoChecksum) {
		t.Errorf("no checksum: %v", err)
	}
	noExe := testRelease()
	noExe.Installer = nil
	if _, err := (Downloader{HTTP: c}).Installer(context.Background(), noExe, t.TempDir(), nil); !errors.Is(err, ErrNoInstaller) {
		t.Errorf("no installer: %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests", hits.Load())
	}
}

func TestDownloadRefusesAURLOutsideTheRelease(t *testing.T) {
	var hits atomic.Int32
	c := serverClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	r := testRelease()
	r.Installer.URL = "https://evil.example/x.exe"
	if _, err := (Downloader{HTTP: c}).Installer(context.Background(), r, t.TempDir(), nil); !errors.Is(err, ErrNoInstaller) {
		t.Errorf("%v", err)
	}
	r = testRelease()
	r.Checksum.URL = "https://github.com/Jang-Luke/sugar-it/releases/x/" + ChecksumAsset
	if _, err := (Downloader{HTTP: c}).Installer(context.Background(), r, t.TempDir(), nil); !errors.Is(err, ErrNoChecksum) {
		t.Errorf("%v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests", hits.Load())
	}
}

func TestRedirects(t *testing.T) {
	data := []byte("installer")
	mux := http.NewServeMux()
	mux.HandleFunc("/Jang-Luke/sugar-it/releases/download/v1.2.4/"+ChecksumAsset, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, sumLine(data, InstallerAsset))
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { w.Write(data) })
	target := "https://release-assets.example/ok"
	mux.HandleFunc("/Jang-Luke/sugar-it/releases/download/v1.2.4/"+InstallerAsset, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	})
	c := serverClient(t, mux)
	if _, err := (Downloader{HTTP: c}).Installer(context.Background(), testRelease(), t.TempDir(), nil); err != nil {
		t.Fatalf("https redirect: %v", err)
	}
	target = "http://release-assets.example/ok"
	dir := t.TempDir()
	if _, err := (Downloader{HTTP: c}).Installer(context.Background(), testRelease(), dir, nil); !errors.Is(err, ErrInsecure) {
		t.Fatalf("http redirect: %v", err)
	}
	emptyDir(t, dir)
}

func TestSizeLimit(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 2048)
	h := serverClient(t, assetServer(data, sumLine(data, InstallerAsset)))
	dir := t.TempDir()
	_, err := Downloader{HTTP: h, MaxSize: 1024}.Installer(context.Background(), testRelease(), dir, nil)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("stream over the limit: %v", err)
	}
	emptyDir(t, dir)

	big := testRelease()
	big.Installer.Size = MaxInstallerSize + 1
	if _, err := (Downloader{HTTP: h}).Installer(context.Background(), big, dir, nil); !errors.Is(err, ErrTooLarge) {
		t.Errorf("declared over the limit: %v", err)
	}
	emptyDir(t, dir)
	if MaxInstallerSize != 500<<20 {
		t.Errorf("MaxInstallerSize = %d", MaxInstallerSize)
	}
}

func TestServerErrorAndStall(t *testing.T) {
	c := serverClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Base(r.URL.Path) == ChecksumAsset {
			fmt.Fprintf(w, "%064x  %s\n", 0, InstallerAsset)
			return
		}
		w.WriteHeader(500)
	}))
	dir := t.TempDir()
	if _, err := (Downloader{HTTP: c}).Installer(context.Background(), testRelease(), dir, nil); err == nil {
		t.Error("500 accepted")
	}
	emptyDir(t, dir)

	release := make(chan struct{})
	defer close(release)
	stalled := serverClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Base(r.URL.Path) == ChecksumAsset {
			fmt.Fprintf(w, "%064x  %s\n", 0, InstallerAsset)
			return
		}
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	start := time.Now()
	dir = t.TempDir()
	if _, err := (Downloader{HTTP: stalled, Stall: 100 * time.Millisecond}).Installer(context.Background(), testRelease(), dir, nil); err == nil {
		t.Error("a stalled download succeeded")
	}
	if time.Since(start) > 20*time.Second {
		t.Error("the stall limit did not apply")
	}
	emptyDir(t, dir)
}

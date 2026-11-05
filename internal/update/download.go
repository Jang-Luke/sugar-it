package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultStall = 60 * time.Second
	maxSumFile   = 4 << 10
)

var (
	// ErrNoInstaller: the release has no installer at the expected URL.
	ErrNoInstaller = errors.New("the release has no installer")
	// ErrNoChecksum: the checksum file is missing or is not one checksum for the installer.
	ErrNoChecksum = errors.New("the release has no usable checksum for the installer")
	// ErrChecksumMismatch: the installer is not what the checksum file says.
	ErrChecksumMismatch = errors.New("the installer does not match its checksum")
	// ErrTooLarge: the installer is over the size limit.
	ErrTooLarge = errors.New("the installer is too large")
)

// sumRE is one sha256sum line; "*" is what some builds print for binary mode.
var sumRE = regexp.MustCompile(`^([0-9a-fA-F]{64}) [ *](\S+)$`)

// Downloader fetches the Windows installer. Its client has no overall time limit,
// because a download can be long; Stall limits how long nothing may arrive.
type Downloader struct {
	HTTP    *http.Client
	Stall   time.Duration // 0 means a minute
	MaxSize int64         // 0 means MaxInstallerSize
}

// Downloaded is a verified installer. The helper checks SHA256 again just before it
// runs the file, which stays in a temporary folder through the app's exit and UAC.
type Downloaded struct{ Path, SHA256 string }

// Installer downloads rel's installer into dir and verifies it. On any failure no
// file is left. progress, if set, is called from this goroutine as bytes arrive.
func (d Downloader) Installer(ctx context.Context, rel Release, dir string, progress func(done, total int64)) (Downloaded, error) {
	max := d.MaxSize
	if max == 0 {
		max = MaxInstallerSize
	}
	if rel.Installer == nil || rel.Installer.URL != assetURL(rel.Tag, InstallerAsset) {
		return Downloaded{}, ErrNoInstaller
	}
	if rel.Checksum == nil || rel.Checksum.URL != assetURL(rel.Tag, ChecksumAsset) {
		return Downloaded{}, ErrNoChecksum
	}
	if rel.Installer.Size > max {
		return Downloaded{}, ErrTooLarge
	}
	want, err := d.checksum(ctx, rel.Checksum.URL)
	if err != nil {
		return Downloaded{}, err
	}
	path := filepath.Join(dir, InstallerAsset)
	if err := d.fetch(ctx, rel.Installer, path, want, max, progress); err != nil {
		os.Remove(path)
		return Downloaded{}, err
	}
	return Downloaded{Path: path, SHA256: want}, nil
}

func (d Downloader) checksum(ctx context.Context, url string) (string, error) {
	var b strings.Builder
	err := d.stream(ctx, url, func(body io.Reader) error {
		_, err := io.Copy(&b, io.LimitReader(body, maxSumFile+1))
		return err
	})
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoChecksum, err)
	}
	if b.Len() > maxSumFile {
		return "", ErrNoChecksum
	}
	line := strings.TrimRight(b.String(), "\r\n")
	m := sumRE.FindStringSubmatch(line)
	if m == nil || m[2] != InstallerAsset {
		return "", ErrNoChecksum
	}
	return strings.ToLower(m[1]), nil
}

func (d Downloader) fetch(ctx context.Context, a *Asset, path, want string, max int64, progress func(done, total int64)) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	err = d.stream(ctx, a.URL, func(body io.Reader) error {
		w := io.MultiWriter(f, h)
		var done int64
		buf := make([]byte, 256<<10)
		for {
			n, rerr := body.Read(buf)
			if n > 0 {
				if done += int64(n); done > max {
					return ErrTooLarge
				}
				if _, err := w.Write(buf[:n]); err != nil {
					return err
				}
				if progress != nil {
					progress(done, a.Size)
				}
			}
			if rerr == io.EOF {
				return nil
			}
			if rerr != nil {
				return rerr
			}
		}
	})
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return ErrChecksumMismatch
	}
	return nil
}

// stream GETs url and hands the body to read, cancelling if nothing arrives in time.
func (d Downloader) stream(ctx context.Context, url string, read func(io.Reader) error) error {
	stall := d.Stall
	if stall == 0 {
		stall = defaultStall
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(stall, cancel)
	defer timer.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "sugar-it-desktop")
	resp, err := httpsOnly(d.HTTP).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	return read(watchdog{resp.Body, timer, stall})
}

// watchdog pushes the stall timer back on every read that returns data.
type watchdog struct {
	r     io.Reader
	timer *time.Timer
	stall time.Duration
}

func (w watchdog) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		w.timer.Reset(w.stall)
	}
	return n, err
}

// Package update finds newer releases of the desktop app and, on Windows, installs
// them: Checker asks GitHub, Downloader fetches and verifies the installer, and
// RunHelper replaces the running program from a copy of it. The desktop app does
// the rest (spec section 11). Release data is untrusted: only a validated tag and
// exact URLs built from it are ever used, and release text is never read.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	repo = "Jang-Luke/sugar-it"

	// APIBase is where releases are looked up.
	APIBase = "https://api.github.com"
	// ReleasesURL is the only prefix the app ever opens in a browser.
	ReleasesURL = "https://github.com/" + repo + "/releases/"
	// DownloadURL is the only prefix the app ever downloads from.
	DownloadURL = ReleasesURL + "download/"

	// InstallerAsset is the Windows installer; ChecksumAsset is its checksum.
	InstallerAsset = "sugar-it-desktop_windows_amd64_installer.exe"
	ChecksumAsset  = InstallerAsset + ".sha256"

	// MaxInstallerSize is the largest installer the app accepts.
	MaxInstallerSize int64 = 500 << 20

	CheckTimeout  = 15 * time.Second
	CheckInterval = 6 * time.Hour

	maxAPIBody = 8 << 20
)

var (
	// ErrDevBuild: this build has no release version, so nothing is checked.
	ErrDevBuild = errors.New("this build has no release version")
	// ErrNoRelease: GitHub knows no full release (404).
	ErrNoRelease = errors.New("no release is published")
	// ErrBadRelease: the release's tag is not a plain vX.Y.Z.
	ErrBadRelease = errors.New("the release has an unexpected tag")
	// ErrInsecure: a redirect left https.
	ErrInsecure = errors.New("refused a redirect that is not https")
)

var (
	tagRE     = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)
	versionRE = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)$`)
)

// Version is a plain three-part version.
type Version struct{ Major, Minor, Patch uint64 }

// Newer reports whether v is strictly newer than o.
func (v Version) Newer(o Version) bool {
	if v.Major != o.Major {
		return v.Major > o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor > o.Minor
	}
	return v.Patch > o.Patch
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// ParseTag parses a release tag, which must match ^v\d+\.\d+\.\d+$.
func ParseTag(tag string) (Version, bool) { return parse(tagRE, tag) }

// ParseVersion parses the version a release build carries: x.y.z, without the v.
func ParseVersion(s string) (Version, bool) { return parse(versionRE, s) }

func parse(re *regexp.Regexp, s string) (Version, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return Version{}, false
	}
	var parts [3]uint64
	for i := range parts {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return Version{}, false
		}
		parts[i] = n
	}
	return Version{parts[0], parts[1], parts[2]}, true
}

// IsReleaseURL reports whether u may be opened in a browser: it has the exact
// prefix and nothing that could lead elsewhere once a browser resolves it.
func IsReleaseURL(u string) bool {
	if !strings.HasPrefix(u, ReleasesURL) {
		return false
	}
	rest := u[len(ReleasesURL):]
	if strings.Contains(rest, "..") || strings.ContainsAny(rest, `%\?#`) {
		return false
	}
	for _, r := range rest {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// Asset is one downloadable file of a release.
type Asset struct {
	URL  string
	Size int64
}

// Release is what the app keeps of a release. An asset is nil unless the release
// lists it at the exact URL it must have.
type Release struct {
	Tag                 string
	Version             Version
	Installer, Checksum *Asset
}

// Installable reports whether the one-click install has what it needs.
func (r Release) Installable() bool { return r.Installer != nil && r.Checksum != nil }

// PageURL is the release's page, built from the validated tag.
func (r Release) PageURL() string { return ReleasesURL + "tag/" + r.Tag }

func assetURL(tag, name string) string { return DownloadURL + tag + "/" + name }

// Checker asks GitHub for the latest full release. /latest already excludes
// drafts and pre-releases. It never retries: the caller's schedule is the retry.
type Checker struct {
	BaseURL string
	HTTP    *http.Client
	Current string        // this build's version
	Timeout time.Duration // 0 means CheckTimeout
}

// Enabled reports whether this build has a release version to compare with.
func (c Checker) Enabled() bool {
	_, ok := ParseVersion(c.Current)
	return ok
}

// Check returns the latest release and whether it is strictly newer than this build.
func (c Checker) Check(ctx context.Context) (Release, bool, error) {
	cur, ok := ParseVersion(c.Current)
	if !ok {
		return Release{}, false, ErrDevBuild
	}
	rel, err := c.latest(ctx)
	if err != nil {
		return Release{}, false, err
	}
	return rel, rel.Version.Newer(cur), nil
}

func (c Checker) latest(ctx context.Context) (Release, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = CheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sugar-it-desktop/"+c.Current)
	resp, err := httpsOnly(c.HTTP).Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return Release{}, ErrNoRelease
	default:
		// Not the body: it is untrusted text and may reach the window.
		return Release{}, fmt.Errorf("GitHub answered %d", resp.StatusCode)
	}
	var body struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIBody)).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("read the release: %w", err)
	}
	v, ok := ParseTag(body.Tag)
	if !ok {
		return Release{}, ErrBadRelease
	}
	rel := Release{Tag: body.Tag, Version: v}
	for _, a := range body.Assets {
		if a.URL != assetURL(rel.Tag, a.Name) {
			continue
		}
		switch a.Name {
		case InstallerAsset:
			rel.Installer = &Asset{URL: a.URL, Size: a.Size}
		case ChecksumAsset:
			rel.Checksum = &Asset{URL: a.URL, Size: a.Size}
		}
	}
	return rel, nil
}

// httpsOnly is c, or a default client, refusing any redirect that leaves https.
func httpsOnly(c *http.Client) *http.Client {
	out := http.Client{}
	if c != nil {
		out = *c
	}
	out.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return ErrInsecure
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return &out
}

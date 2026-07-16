// Package selfupdate replaces the running binary with the latest GitHub
// release, verifying the download against the release's checksums.txt.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	repoOwner = "jclement"
	repoName  = "restic-duper"
	// maxDownload caps release downloads; our archives are a few MiB.
	maxDownload = 200 << 20
)

// apiBase is a variable so tests can point at a stub server.
var apiBase = "https://api.github.com"

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type release struct {
	Tag    string  `json:"tag_name"`
	Assets []asset `json:"assets"`
}

// Latest returns the newest release tag and its assets.
func Latest(ctx context.Context, client *http.Client) (*release, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/repos/%s/%s/releases/latest", apiBase, repoOwner, repoName), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "restic-duper-self-update")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parsing release metadata: %w", err)
	}
	if rel.Tag == "" {
		return nil, fmt.Errorf("release metadata has no tag")
	}
	return &rel, nil
}

func (r *release) assetURL(name string) (string, error) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("release %s has no asset %q", r.Tag, name)
}

// ArchiveName returns the goreleaser archive name for a version tag and
// platform, e.g. restic-duper_0.4.0_linux_amd64.tar.gz.
func ArchiveName(tag, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s.%s", repoName, strings.TrimPrefix(tag, "v"), goos, goarch, ext)
}

// VerifyChecksum checks data against the entry for name in a goreleaser
// checksums.txt ("<sha256>  <filename>" lines).
func VerifyChecksum(checksums []byte, name string, data []byte) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			if !strings.EqualFold(fields[0], got) {
				return fmt.Errorf("checksum mismatch for %s: expected %s, downloaded file has %s", name, fields[0], got)
			}
			return nil
		}
	}
	return fmt.Errorf("checksums.txt has no entry for %s", name)
}

// ExtractBinary pulls the restic-duper executable out of a release archive.
func ExtractBinary(archiveName string, data []byte) ([]byte, error) {
	want := repoName
	if strings.HasSuffix(archiveName, ".zip") {
		want += ".exe"
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == want {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, maxDownload))
			}
		}
		return nil, fmt.Errorf("archive has no %s", want)
	}

	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("archive has no %s", want)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == want {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

func download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "restic-duper-self-update")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload))
}

// ReplaceExecutable atomically installs newBinary over target: the new file
// is written next to it and renamed into place, so a crash mid-update never
// leaves a half-written executable.
func ReplaceExecutable(target string, newBinary []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".restic-duper-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (try with elevated privileges?): %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(newBinary); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// Windows cannot replace a running executable in place; move it
		// aside first.
		old := target + ".old"
		os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			return err
		}
	}
	return os.Rename(tmpName, target)
}

// Update installs the latest release over the current executable. Returns
// the tag installed, or "" if already up to date.
func Update(ctx context.Context, log *slog.Logger, currentVersion string, checkOnly bool) (string, error) {
	client := &http.Client{Timeout: 5 * time.Minute}

	rel, err := Latest(ctx, client)
	if err != nil {
		return "", fmt.Errorf("finding latest release: %w", err)
	}
	if normalize(rel.Tag) == normalize(currentVersion) {
		log.Info("already up to date", "version", rel.Tag)
		return "", nil
	}
	log.Info("update available", "current", currentVersion, "latest", rel.Tag)
	if checkOnly {
		return rel.Tag, nil
	}

	name := ArchiveName(rel.Tag, runtime.GOOS, runtime.GOARCH)
	archiveURL, err := rel.assetURL(name)
	if err != nil {
		return "", err
	}
	sumsURL, err := rel.assetURL("checksums.txt")
	if err != nil {
		return "", err
	}

	log.Info("downloading", "asset", name)
	archive, err := download(ctx, client, archiveURL)
	if err != nil {
		return "", err
	}
	sums, err := download(ctx, client, sumsURL)
	if err != nil {
		return "", err
	}
	if err := VerifyChecksum(sums, name, archive); err != nil {
		return "", err
	}
	log.Debug("checksum verified", "asset", name)

	binary, err := ExtractBinary(name, archive)
	if err != nil {
		return "", err
	}

	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if err := ReplaceExecutable(exe, binary); err != nil {
		return "", err
	}
	log.Info("updated", "path", exe, "version", rel.Tag)
	return rel.Tag, nil
}

func normalize(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

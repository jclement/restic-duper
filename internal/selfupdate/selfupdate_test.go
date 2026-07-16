package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func makeTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestArchiveName(t *testing.T) {
	if got := ArchiveName("v0.4.0", "linux", "amd64"); got != "restic-duper_0.4.0_linux_amd64.tar.gz" {
		t.Errorf("ArchiveName = %s", got)
	}
	if got := ArchiveName("v0.4.0", "windows", "arm64"); got != "restic-duper_0.4.0_windows_arm64.zip" {
		t.Errorf("ArchiveName = %s", got)
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("hello release")
	sum := sha256.Sum256(data)
	sums := []byte(fmt.Sprintf("%s  restic-duper_0.4.0_linux_amd64.tar.gz\nother  other.zip\n", hex.EncodeToString(sum[:])))

	if err := VerifyChecksum(sums, "restic-duper_0.4.0_linux_amd64.tar.gz", data); err != nil {
		t.Errorf("valid checksum rejected: %v", err)
	}
	if err := VerifyChecksum(sums, "restic-duper_0.4.0_linux_amd64.tar.gz", []byte("tampered")); err == nil {
		t.Error("tampered data must fail verification")
	}
	if err := VerifyChecksum(sums, "missing.tar.gz", data); err == nil {
		t.Error("missing entry must fail verification")
	}
}

func TestExtractBinary(t *testing.T) {
	want := []byte("#!/fake-binary")
	archive := makeTarGz(t, "restic-duper", want)
	got, err := ExtractBinary("restic-duper_0.4.0_linux_amd64.tar.gz", archive)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("extracted %q", got)
	}
	if _, err := ExtractBinary("x.tar.gz", makeTarGz(t, "README.md", []byte("nope"))); err == nil {
		t.Error("archive without binary must error")
	}
}

func TestReplaceExecutable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "restic-duper")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceExecutable(target, []byte("new")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "new" {
		t.Errorf("target = %q", got)
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("replaced binary must be executable")
	}
}

// Full update flow against a stubbed GitHub API.
func TestUpdateFlow(t *testing.T) {
	binary := []byte("#!/new-shiny-binary")
	archiveName := ArchiveName("v9.9.9", "linux", "amd64")
	archive := makeTarGz(t, "restic-duper", binary)
	sum := sha256.Sum256(archive)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), archiveName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/jclement/restic-duper/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[
				{"name":"%s","browser_download_url":"%s/dl/archive"},
				{"name":"checksums.txt","browser_download_url":"%s/dl/sums"}]}`,
				archiveName, "http://"+r.Host, "http://"+r.Host)
		case r.URL.Path == "/dl/archive":
			w.Write(archive)
		case r.URL.Path == "/dl/sums":
			fmt.Fprint(w, sums)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	ctx := context.Background()
	rel, err := Latest(ctx, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v9.9.9" {
		t.Fatalf("tag = %s", rel.Tag)
	}
	url, err := rel.assetURL(archiveName)
	if err != nil {
		t.Fatal(err)
	}
	data, err := download(ctx, srv.Client(), url)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksum([]byte(sums), archiveName, data); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractBinary(archiveName, data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binary) {
		t.Errorf("extracted %q", got)
	}
}

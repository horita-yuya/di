// Package upgrade replaces the running di binary with the latest GitHub release.
package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

const releases = "https://github.com/horita-yuya/di/releases"

func Run(current string) error {
	if runtime.GOOS == "windows" {
		return errors.New("upgrade is not supported on Windows; download the latest release from " + releases)
	}
	// releases/latest redirects to .../tag/<tag>, so the tag is found without the API.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(releases + "/latest")
	if err != nil {
		return err
	}
	resp.Body.Close()
	_, tag, ok := strings.Cut(resp.Header.Get("Location"), "/tag/")
	if resp.StatusCode != http.StatusFound || !ok {
		return fmt.Errorf("GET %s/latest: %s", releases, resp.Status)
	}
	if tag == "v"+current {
		fmt.Printf("di is up to date (%s)\n", tag)
		return nil
	}

	name := fmt.Sprintf("di_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive, err := get(releases + "/download/" + tag + "/" + name)
	if err != nil {
		return err
	}
	sums, err := get(releases + "/download/" + tag + "/checksums.txt")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	if !slices.Contains(strings.Split(string(sums), "\n"), hex.EncodeToString(sum[:])+"  "+name) {
		return fmt.Errorf("checksum mismatch for %s", name)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return fmt.Errorf("di not found in %s: %w", name, err)
		}
		if h.Name == "di" {
			break
		}
	}

	// Write next to the binary and rename over it, so the swap is atomic.
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".di-upgrade-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	_, err = io.Copy(tmp, tr)
	if err := errors.Join(err, tmp.Chmod(0o755), tmp.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return err
	}
	fmt.Printf("upgraded di %s → %s\n", current, tag)
	return nil
}

func get(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

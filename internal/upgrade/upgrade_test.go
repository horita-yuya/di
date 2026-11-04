package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

var asset = fmt.Sprintf("di_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

func targz(t *testing.T, files ...string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i < len(files); i += 2 {
		tw.WriteHeader(&tar.Header{Name: files[i], Mode: 0o755, Size: int64(len(files[i+1]))})
		tw.Write([]byte(files[i+1]))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func checksum(name string, data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) + "  " + name + "\n"
}

// fakeGitHub serves a release tagged tag and routes every request made through http.DefaultTransport to it.
// It returns the paths requested.
func fakeGitHub(t *testing.T, tag string, archive []byte, sums string) func() []string {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/horita-yuya/di/releases/latest":
			http.Redirect(w, r, "https://github.com/horita-yuya/di/releases/tag/"+tag, http.StatusFound)
		case "/horita-yuya/di/releases/download/" + tag + "/" + asset:
			w.Write(archive)
		case "/horita-yuya/di/releases/download/" + tag + "/checksums.txt":
			w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	orig := http.DefaultTransport
	http.DefaultTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = "http", srv.Listener.Addr().String()
		return orig.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = orig })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(paths)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func skipWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("upgrade is not supported on Windows")
	}
}

func TestUpToDate(t *testing.T) {
	skipWindows(t)
	paths := fakeGitHub(t, "v1.2.3", nil, "")
	if err := Run("1.2.3"); err != nil {
		t.Fatal(err)
	}
	if got := paths(); len(got) != 1 {
		t.Fatalf("downloaded although up to date: %v", got)
	}
}

func TestRejected(t *testing.T) {
	skipWindows(t)
	archive := targz(t, "di", "new binary")
	for name, tc := range map[string]struct {
		archive []byte
		sums    string
		want    string
	}{
		"checksum mismatch":         {archive, checksum(asset, []byte("something else")), "checksum mismatch"},
		"checksum of another asset": {archive, checksum("di_plan9_386.tar.gz", archive), "checksum mismatch"},
		"no di in the archive":      {targz(t, "README.md", "hi"), "", "di not found"},
	} {
		t.Run(name, func(t *testing.T) {
			sums := tc.sums
			if sums == "" {
				sums = checksum(asset, tc.archive)
			}
			fakeGitHub(t, "v1.2.4", tc.archive, sums)
			exe := executable(t)
			before, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			if err := Run("1.2.3"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run = %v, want %q", err, tc.want)
			}
			if after, _ := os.ReadFile(exe); !bytes.Equal(before, after) {
				t.Fatal("the binary was replaced")
			}
		})
	}
}

// TestReplace overwrites the running test binary, so it only runs when that is a throwaway file under the temp dir.
func TestReplace(t *testing.T) {
	skipWindows(t)
	exe := executable(t)
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil || !strings.HasPrefix(exe, tmp+string(filepath.Separator)) {
		t.Skipf("test binary %s is not under %s", exe, tmp)
	}
	archive := targz(t, "LICENSE", "license", "README.md", "readme", "di", "new binary", "completions/di.bash", "completion")
	fakeGitHub(t, "v1.2.4", archive, checksum("di_other_arch.tar.gz", []byte("x"))+checksum(asset, archive))
	if err := Run("1.2.3"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != "new binary" {
		t.Fatalf("binary = %q, %v", got, err)
	}
	if fi, err := os.Stat(exe); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("binary not executable: %v %v", fi.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".di-upgrade-*")); len(left) > 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func executable(t *testing.T) string {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

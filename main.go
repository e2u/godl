package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/e2u/e2util/e2env"
	"github.com/e2u/e2util/e2http"
	"github.com/pkg/errors"
	"golang.org/x/sys/execabs"
)

var (
	unstable bool
	dryRun   bool
)

func main() {
	e2env.EnvBoolVar(&unstable, "unstable", false, "list unstable releases")
	e2env.EnvBoolVar(&dryRun, "dryrun", true, "download go install package and extract to /tmp/go directory, not actually install")
	flag.Parse()

	goRoot := os.Getenv("GOROOT")
	if goRoot == "" {
		fmt.Fprintf(os.Stderr, "GOROOT must be set.")
		return
	} else {
		fmt.Printf("GOROOT: %s\n", goRoot)
	}

	installedVersion, err := getInstalledVersion()
	if err != nil {
		fmt.Fprintf(os.Stderr, "GetInstalledVersion error: %s\n", err)
		return
	}

	latestRelease, err := getNewVersionFile(context.TODO(), getReleases, installedVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		return
	}
	downloadUrl := fmt.Sprintf("https://dl.google.com/go/%s", latestRelease.Filename)
	fmt.Println("downloading: ", downloadUrl)

	f, err := os.CreateTemp(os.TempDir(), filepath.Base(downloadUrl))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		return
	}
	defer os.Remove(f.Name())

	errs := e2http.Builder(context.TODO()).URL(downloadUrl).Write(f).Do().Errors()
	if len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "download install package error: %s\n", errs)
		return
	}
	f.Close()

	r, err := os.Open(f.Name())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		return
	}

	if err := extractTarGz(r, "/tmp/"); err != nil {
		fmt.Fprintf(os.Stderr, "extract tar.gz error: %s\n", err)
		return
	}

	if dryRun {
		fmt.Fprintf(os.Stdout, "not actually install...\n")
		return
	}

	if err := os.Rename(goRoot, goRoot+"@"+installedVersion.Version); err != nil {
		fmt.Fprintf(os.Stderr, "rename error: %v %v %v\n", goRoot, goRoot+"@installedVersion.Version", err)
		return
	}

	if err := os.Rename("/tmp/go", goRoot); err != nil {
		fmt.Fprintf(os.Stderr, "rename error: %v %v\n", err, err)
		return
	}

}

type File struct {
	Filename string `json:"filename"`
	Os       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
	Sha256   string `json:"sha256"`
	Size     int    `json:"size"`
	Kind     string `json:"kind"`
}

type Release struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
	Files   []File `json:"files"`
}

type InstalledVersion struct {
	Os      string `json:"os"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
}

func getNewVersionFile(ctx context.Context, fn func(ctx context.Context) ([]Release, error), iv InstalledVersion) (File, error) {
	releases, err := fn(ctx)
	if err != nil {
		return File{}, err
	}

	for _, release := range releases {
		if !release.Stable {
			continue
		}
		for _, file := range release.Files {
			if file.Os == iv.Os && iv.Arch == file.Arch && versionGreater(file.Version, iv.Version) {
				return file, nil
			}
		}
	}
	return File{}, errors.New("no new version file found")
}

func getReleases(ctx context.Context) ([]Release, error) {
	var rs []Release
	if errs := e2http.Builder(ctx).
		URL("https://go.dev/dl/?mode=json&include=all").
		ToJSON(&rs).
		Do().Errors(); len(errs) > 0 {
		return nil, errs[0]
	}
	sort.Slice(rs, func(i, j int) bool {
		return versionLess(rs[i].Version, rs[j].Version)
	})
	return rs, nil
}

func versionLess(a, b string) bool {
	maja, mina, ta := parseVersion(a)
	majb, minb, tb := parseVersion(b)
	if maja == majb {
		if mina == minb {
			if ta == "" {
				return true
			} else if tb == "" {
				return false
			}
			return ta >= tb
		}
		return mina >= minb
	}
	return maja >= majb
}

// is a > b
func versionGreater(a, b string) bool {
	maja, mina, ta := parseVersion(a)
	majb, minb, tb := parseVersion(b)
	if maja == majb {
		if mina == minb && (ta != "" || tb != "") {
			if ta == "" {
				return true
			} else if tb == "" {
				return false
			}
			return ta > tb
		}
		return mina > minb
	}
	return maja > majb
}

func parseVersion(v string) (maj, min int, tail string) {
	if i := strings.Index(v, "beta"); i > 0 {
		tail = v[i:]
		v = v[:i]
	}
	if i := strings.Index(v, "rc"); i > 0 {
		tail = v[i:]
		v = v[:i]
	}
	p := strings.Split(strings.TrimPrefix(v, "go1."), ".")
	maj, _ = strconv.Atoi(p[0])
	if len(p) < 2 {
		return
	}
	min, _ = strconv.Atoi(p[1])
	return
}

func getInstalledVersion() (InstalledVersion, error) {
	c := execabs.Command("go", "version")
	out, err := c.Output()
	if err != nil {
		return InstalledVersion{}, err
	}
	vs := strings.Split(string(out), " ")
	if len(vs) < 2 {
		return InstalledVersion{}, errors.New("invalid go version")
	}
	oa := strings.Split(vs[3], "/")
	return InstalledVersion{
		Os:   strings.TrimSpace(oa[0]),
		Arch: strings.TrimSpace(oa[1]),
		//Version: "go1.22.0", //vs[2],
		Version: strings.TrimSpace(vs[2]),
	}, nil
}

func extractTarGz(gr io.Reader, baseDir string) error {
	gzr, err := gzip.NewReader(gr)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(filepath.Join(baseDir, header.Name), 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := func(header *tar.Header, tr io.Reader) error {
				outFile, err := os.OpenFile(filepath.Join(baseDir, header.Name), os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
				if err != nil {
					return err
				}
				defer outFile.Close()
				if _, err := io.Copy(outFile, tr); err != nil {
					return err
				}
				return nil
			}(header, tr); err != nil {
				return err
			}
		default:
			slog.Error("unknown type:", "type", header.Typeflag, "name", header.Name)
		}
	}
	return nil
}

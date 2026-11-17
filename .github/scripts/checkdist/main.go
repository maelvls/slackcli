// Command checkdist verifies that the archives GoReleaser left in a dist
// directory are the ones self-update (internal/update) and install.sh
// download: named by update.AssetName, holding the binary named by
// update.BinaryName at their root, and listed with their SHA-256 in
// update.ChecksumsName in sha256sum format.
//
// CI runs it after `goreleaser release --snapshot`:
//
//	go run ./.github/scripts/checkdist dist
//
// The platforms come from the artifacts.json GoReleaser writes and the
// names from internal/update, so nothing here repeats the target matrix
// or the naming scheme.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/maelvls/slackcli/internal/update"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: checkdist <dist directory>")
		os.Exit(2)
	}
	problems, err := check(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "checkdist:", err)
		os.Exit(1)
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "checkdist:", p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
}

// artifact is an entry of GoReleaser's dist/artifacts.json.
type artifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Goos   string `json:"goos"`
	Goarch string `json:"goarch"`
	Type   string `json:"type"`
}

// check returns what in dir breaks the release naming contract, and prints
// what it verified.
func check(dir string) (problems []string, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, "artifacts.json"))
	if err != nil {
		return nil, fmt.Errorf("%w (was `goreleaser release --snapshot` run?)", err)
	}
	var artifacts []artifact
	if err := json.Unmarshal(raw, &artifacts); err != nil {
		return nil, fmt.Errorf("decoding artifacts.json: %w", err)
	}
	sums, err := readChecksums(filepath.Join(dir, update.ChecksumsName))
	if err != nil {
		return nil, err
	}
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	// What a release publishes: the archives and the checksums file.
	published := map[string]bool{update.ChecksumsName: true}
	platforms := map[string]bool{}
	for _, a := range artifacts {
		if a.Type != "Archive" {
			continue
		}
		platforms[a.Goos+"/"+a.Goarch] = true
		want := update.AssetName(a.Goos, a.Goarch)
		if a.Name != want {
			bad("%s/%s is archived as %s, self-update and install.sh download %s", a.Goos, a.Goarch, a.Name, want)
			continue
		}
		published[want] = true
		before := len(problems)
		content, err := os.ReadFile(filepath.Join(dir, want))
		if err != nil {
			bad("%v", err)
			continue
		}
		binary := update.BinaryName(a.Goos)
		names, err := archiveEntries(want, content)
		switch {
		case err != nil:
			bad("%s: %v", want, err)
		case !slices.Contains(names, binary):
			bad("%s holds %v: the binary %q is not at its root", want, names, binary)
		}
		sum := sha256.Sum256(content)
		switch got, listed := sums[want]; {
		case !listed:
			bad("%s has no entry for %s", update.ChecksumsName, want)
		case got != hex.EncodeToString(sum[:]):
			bad("%s lists %s for %s, whose SHA-256 is %x", update.ChecksumsName, got, want, sum)
		}
		if len(problems) == before {
			fmt.Printf("ok  %s (%s at the root, checksum listed)\n", want, binary)
		}
	}
	if len(platforms) == 0 {
		bad("artifacts.json lists no archive")
	}
	// The machine running this is one a release must cover; it guards
	// against a build matrix that lost its platforms.
	if host := runtime.GOOS + "/" + runtime.GOARCH; !platforms[host] {
		bad("no archive was built for %s, the platform running this check", host)
	}

	for name := range sums {
		if !published[name] {
			bad("%s lists %s, which is not a release archive", update.ChecksumsName, name)
		}
	}
	// Nothing else may look like a release asset: a stray archive would
	// be uploaded next to the expected ones.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		isAsset := strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".txt")
		if !e.IsDir() && isAsset && !published[name] {
			bad("unexpected release asset %s in %s", name, dir)
		}
	}
	sort.Strings(problems)
	return problems, nil
}

// checksumLine is a line of sha256sum's output, for a file read in text
// ("  ") or binary (" *") mode.
var checksumLine = regexp.MustCompile(`^([0-9a-f]{64}) [ *]([^ ].*)$`)

// readChecksums parses a sha256sum-style listing into name -> hex digest,
// refusing any line that sha256sum -c, install.sh or self-update would not
// read the same way.
func readChecksums(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		m := checksumLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("%s: line %q is not in sha256sum format (\"<64 hex digits>  <name>\")", path, line)
		}
		sums[m[2]] = m[1]
	}
	return sums, nil
}

// archiveEntries lists the paths of the files in a .tar.gz or .zip.
func archiveEntries(name string, content []byte) ([]string, error) {
	var names []string
	if strings.HasSuffix(name, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		return names, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		names = append(names, h.Name)
	}
}

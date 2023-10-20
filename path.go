package runn

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/k1LoW/ghfs"
	"github.com/k1LoW/urlfilepath"
)

const (
	schemeHttps  = "https"
	schemeGitHub = "github"
)

const (
	prefixHttps  = schemeHttps + "://"
	prefixGitHub = schemeGitHub + "://"
)

// ShortenPath shorten path.
func ShortenPath(p string) string {
	flags := strings.Split(p, string(filepath.Separator))
	abs := false
	if flags[0] == "" {
		abs = true
	}
	var s []string
	for _, f := range flags[:len(flags)-1] {
		if len(f) > 0 {
			s = append(s, string(f[0]))
		}
	}
	s = append(s, flags[len(flags)-1])
	if abs {
		return string(filepath.Separator) + filepath.Join(s...)
	}
	return filepath.Join(s...)
}

// fetchPaths retrieves readable file paths from path list ( like `path/to/a.yml;path/to/b/**/*.yml` ) .
// If the file paths are remote files, it fetches them and returns their local cache paths.
func fetchPaths(pathp string) ([]string, error) {
	var paths []string
	listp := splitList(pathp)
	for _, pp := range listp {
		base, pattern := doublestar.SplitPattern(filepath.ToSlash(pp))
		switch {
		case strings.HasPrefix(base, prefixHttps):
			// https://
			if strings.Contains(pattern, "*") {
				return nil, fmt.Errorf("https scheme does not support wildcard: %s", pp)
			}
			p, err := fetchPathViaHTTPS(pp)
			if err != nil {
				return nil, err
			}
			paths = append(paths, p)
		case strings.HasPrefix(base, prefixGitHub):
			// github://
			splitted := strings.Split(strings.TrimPrefix(base, prefixGitHub), "/")
			if len(splitted) < 2 {
				return nil, fmt.Errorf("invalid path: %s", pp)
			}
			owner := splitted[0]
			repo := splitted[1]
			sub := splitted[2:]
			gfs, err := ghfs.New(owner, repo)
			if err != nil {
				return nil, err
			}
			var fsys fs.FS
			if len(sub) > 0 {
				fsys, err = gfs.Sub(strings.Join(sub, "/"))
				if err != nil {
					return nil, err
				}
			} else {
				fsys = gfs
			}
			ps, err := fetchPathsViaGitHub(fsys, base, pattern)
			if err != nil {
				return nil, err
			}
			paths = append(paths, ps...)
		default:
			// Local file or cache

			// Local single file
			if !strings.Contains(pattern, "*") {
				if _, err := readFile(pp); err == nil {
					paths = append(paths, pp)
				} // skip if file not found
				continue
			}

			// Local multiple files
			abs, err := filepath.Abs(base)
			if err != nil {
				return nil, err
			}
			fsys := os.DirFS(abs)
			if err := doublestar.GlobWalk(fsys, pattern, func(p string, d fs.DirEntry) error {
				if d.IsDir() {
					return nil
				}
				paths = append(paths, filepath.Join(base, p))
				return nil
			}); err != nil {
				return nil, err
			}
		}
	}
	return unique(paths), nil
}

// fetchPath retrieves readable file path.
func fetchPath(path string) (string, error) {
	paths, err := fetchPaths(path)
	if err != nil {
		return "", err
	}
	if len(paths) > 1 {
		return "", errors.New("multiple paths found")
	}
	if len(paths) == 0 {
		return "", errors.New("path not found")
	}
	return paths[0], nil
}

// fileExists checks if the file exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return err == nil
}

// readFile reads single file from local or cache.
// When retrieving a cache file, if the cache file does not exist, re-fetch it.
func readFile(p string) ([]byte, error) {
	_, err := os.Stat(p)
	if err == nil {
		// Read local file or cache
		return os.ReadFile(p)
	}
	if globalCacheDir == "" || !strings.HasPrefix(p, globalCacheDir) {
		// Not cache file
		return nil, err
	}

	// Re-fetch remote file and create cache
	pathstr, err := filepath.Rel(globalCacheDir, p)
	if err != nil {
		return nil, err
	}
	u, err := urlfilepath.Decode(pathstr)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case schemeHttps:
		b, err := readFileViaHTTPS(u.String())
		if err != nil {
			return nil, err
		}
		// Write cache
		if err := os.WriteFile(p, b, os.ModePerm); err != nil {
			return nil, err
		}
		return b, err
	case schemeGitHub:
		b, err := readFileViaGitHub(u.String())
		if err != nil {
			return nil, err
		}
		// Write cache
		if err := os.WriteFile(p, b, os.ModePerm); err != nil {
			return nil, err
		}
		return b, err
	default:
		return nil, fmt.Errorf("unsupported scheme: %s", u.String())
	}
}

func fetchPathViaHTTPS(urlstr string) (string, error) {
	u, err := url.Parse(urlstr)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	cd, err := cacheDir()
	if err != nil {
		return "", err
	}
	ep, err := urlfilepath.Encode(u)
	if err != nil {
		return "", err
	}
	p := filepath.Join(cd, ep)
	if err := os.MkdirAll(filepath.Dir(p), os.ModePerm); err != nil {
		return "", err
	}
	n, err := os.Create(p)
	if err != nil {
		return "", err
	}
	defer n.Close()
	if _, err = io.Copy(n, res.Body); err != nil {
		return "", err
	}
	return p, nil
}

func fetchPathsViaGitHub(fsys fs.FS, base, pattern string) ([]string, error) {
	var paths []string
	cd, err := cacheDir()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	ep, err := urlfilepath.Encode(u)
	if err != nil {
		return nil, err
	}
	fetchDir := filepath.Join(cd, ep)
	if err := doublestar.GlobWalk(fsys, pattern, func(p string, d fs.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		cp := filepath.Join(fetchDir, p)
		paths = append(paths, cp)

		// Write cache
		f, err := fsys.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()

		if err := os.MkdirAll(filepath.Dir(cp), os.ModePerm); err != nil {
			return err
		}
		n, err := os.Create(cp)
		if err != nil {
			return err
		}
		defer n.Close()
		if _, err := io.Copy(n, f); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return paths, nil
}

func readFileViaHTTPS(urlstr string) ([]byte, error) {
	u, err := url.Parse(urlstr)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(res.Body)
}

func readFileViaGitHub(urlstr string) ([]byte, error) {
	splitted := strings.Split(strings.TrimPrefix(urlstr, prefixGitHub), "/")
	if len(splitted) < 2 {
		return nil, fmt.Errorf("invalid url: %s", urlstr)
	}
	owner := splitted[0]
	repo := splitted[1]
	p := strings.Join(splitted[2:], "/")
	gfs, err := ghfs.New(owner, repo)
	if err != nil {
		return nil, err
	}
	f, err := gfs.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// splitList splits the path list by os.PathListSeparator while keeping schemes.
func splitList(pathp string) []string {
	rep := strings.NewReplacer(prefixHttps, repKey(prefixHttps), prefixGitHub, repKey(prefixGitHub))
	per := strings.NewReplacer(repKey(prefixHttps), prefixHttps, repKey(prefixGitHub), prefixGitHub)
	var listp []string
	for _, p := range filepath.SplitList(rep.Replace(pathp)) {
		listp = append(listp, per.Replace(p))
	}
	return listp
}

func repKey(in string) string {
	return fmt.Sprintf("RUNN_%s_SCHEME", strings.TrimSuffix(strings.ToUpper(in), "://"))
}

func unique(in []string) []string {
	var u []string
	m := map[string]struct{}{}
	for _, s := range in {
		if _, ok := m[s]; ok {
			continue
		}
		u = append(u, s)
		m[s] = struct{}{}
	}
	return u
}

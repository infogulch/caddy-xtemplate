package xtemplate

// These types and methods are used while creating an instance

import (
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template/parse"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type fileInfo struct {
	identityPath, hash, contentType string
	encodings                       []encodingInfo
}

type encodingInfo struct {
	encoding, path string
	size           int64
	modtime        time.Time
}

var extensionContentTypes = map[string]string{
	".css": "text/css; charset=utf-8",
	".js":  "text/javascript; charset=utf-8",
	".csv": "text/csv",
}

func (x *xinstance) addStaticFileHandler(path_ string) error {
	// Open and stat the file
	fsfile, err := x.Template.FS.Open(path_)
	if err != nil {
		return fmt.Errorf("failed to open static file '%s': %w", path_, err)
	}
	defer fsfile.Close()
	seeker := fsfile.(io.ReadSeeker)
	stat, err := fsfile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file '%s': %w", path_, err)
	}
	size := stat.Size()

	var file *fileInfo
	var encoding string
	var sri string
	// Calculate the file hash. If there's a compressed file with the same
	// prefix, calculate the hash of the contents and check that they match.
	ext := filepath.Ext(path_)
	identityPath := strings.TrimSuffix(path.Clean("/"+path_), ext)
	var reader io.Reader = fsfile
	encoding = "identity"
	var exists bool
	file, exists = x.files[identityPath]
	if exists {
		switch ext {
		case ".gz":
			reader, err = gzip.NewReader(seeker)
			encoding = "gzip"
		case ".zst":
			reader, err = zstd.NewReader(seeker)
			encoding = "zstd"
		case ".br":
			reader = brotli.NewReader(seeker)
			encoding = "br"
		}
		if err != nil {
			return fmt.Errorf("failed to create decompressor for file `%s`: %w", path_, err)
		}
	} else {
		identityPath = path.Clean("/" + path_)
		file = &fileInfo{}
	}

	{
		hash := sha512.New384()
		_, err = io.Copy(hash, reader)
		if err != nil {
			return fmt.Errorf("failed to hash file %w", err)
		}
		sri = "sha384-" + base64.URLEncoding.EncodeToString(hash.Sum(nil))
	}

	// Save precalculated file size, modtime, hash, content type, and encoding
	// info to enable efficient content negotiation at request time.
	if encoding == "identity" {
		// note: identity file will always be found first because fs.WalkDir sorts files in lexical order
		file.hash = sri
		file.identityPath = identityPath
		if ctype, ok := extensionContentTypes[ext]; ok {
			file.contentType = ctype
		} else {
			content := make([]byte, 512)
			seeker.Seek(0, io.SeekStart)
			count, err := seeker.Read(content)
			if err != nil && err != io.EOF {
				return fmt.Errorf("failed to read file to guess content type '%s': %w", path_, err)
			}
			file.contentType = http.DetectContentType(content[:count])
		}
		file.encodings = []encodingInfo{{encoding: encoding, path: path_, size: size, modtime: stat.ModTime()}}

		pattern := "GET " + identityPath
		handler := staticFileHandler(x.Template.FS, file)
		x.router.HandleFunc(pattern, handler)
		x.stats.StaticFiles += 1
		x.stats.Routes += 1
		x.files[identityPath] = file
		x.routes = append(x.routes, InstanceRoute{pattern, handler})

		x.Logger.Debug("added static file handler", slog.String("path", identityPath), slog.String("filepath", path_), slog.String("contenttype", file.contentType), slog.Int64("size", size), slog.Time("modtime", stat.ModTime()), slog.String("hash", sri))
	} else {
		if file.hash != sri {
			return fmt.Errorf("encoded file contents did not match original file '%s': expected %s, got %s", path_, file.hash, sri)
		}
		file.encodings = append(file.encodings, encodingInfo{encoding: encoding, path: path_, size: size, modtime: stat.ModTime()})
		sort.Slice(file.encodings, func(i, j int) bool { return file.encodings[i].size < file.encodings[j].size })
		x.stats.StaticFilesAlternateEncodings += 1
		x.Logger.Debug("added static file encoding", slog.String("path", identityPath), slog.String("filepath", path_), slog.String("encoding", encoding), slog.Int64("size", size), slog.Time("modtime", stat.ModTime()))
	}
	return nil
}

var routeMatcher *regexp.Regexp = regexp.MustCompile("^(GET|POST|PUT|PATCH|DELETE|SSE) (.*)$")

func (x *xinstance) addTemplateHandler(path_ string) error {
	content, err := fs.ReadFile(x.Template.FS, path_)
	if err != nil {
		return fmt.Errorf("could not read template file '%s': %v", path_, err)
	}
	if x.Template.Minify {
		content, err = x.minify.Bytes("text/html", content)
		if err != nil {
			return fmt.Errorf("could not minify template file '%s': %v", path_, err)
		}
	}
	path_ = path.Clean("/" + path_)
	// parse each template file manually to have more control over its final
	// names in the template namespace.
	newtemplates, err := parse.Parse(path_, string(content), x.Template.Delimiters.Left, x.Template.Delimiters.Right, x.funcs, buliltinsSkeleton)
	if err != nil {
		return fmt.Errorf("could not parse template file '%s': %v", path_, err)
	}
	x.stats.TemplateFiles += 1
	// add all templates
	for name, tree := range newtemplates {
		if x.templates.Lookup(name) != nil {
			x.Logger.Debug("overriding named template '%s' with definition from file: %s", name, path_)
		}
		tmpl, err := x.templates.AddParseTree(name, tree)
		if err != nil {
			return fmt.Errorf("could not add template '%s' from '%s': %v", name, path_, err)
		}
		x.stats.TemplateDefinitions += 1

		var pattern string
		var handler http.HandlerFunc
		if name == path_ {
			// don't register routes to hidden files
			_, file := filepath.Split(path_)
			if len(file) > 0 && file[0] == '.' {
				continue
			}
			// strip the extension from the handled path
			routePath := strings.TrimSuffix(path_, x.Template.TemplateExtension)
			// files named 'index' handle requests to the directory
			if path.Base(routePath) == "index" {
				routePath = path.Dir(routePath)
			}
			if strings.HasSuffix(routePath, "/") {
				routePath += "{$}"
			}
			pattern = "GET " + routePath
			handler = bufferingTemplateHandler(x, tmpl)
		} else if matches := routeMatcher.FindStringSubmatch(name); len(matches) == 3 {
			method, path_ := matches[1], matches[2]
			if method == "SSE" {
				pattern = "GET " + path_
				handler = flushingTemplateHandler(x, tmpl)
			} else {
				pattern = method + " " + path_
				handler = bufferingTemplateHandler(x, tmpl)
			}
		}

		x.router.HandleFunc(pattern, handler)
		x.routes = append(x.routes, InstanceRoute{pattern, handler})
		x.stats.Routes += 1
		x.Logger.Debug("added template handler", "method", "GET", "pattern", pattern, "template_path", path_)
	}
	return nil
}

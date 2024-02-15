package xtemplate

import (
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"text/template/parse"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type Handler func(w http.ResponseWriter, r *http.Request, log *slog.Logger, server *xtemplate)

var instanceIdentity int64

func (configs *config) Build() (CancelHandler, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// start with default xtemplate struct
	x := &xtemplate{
		templateFS:        os.DirFS("templates"),
		ldelim:            "{{",
		rdelim:            "}}",
		templateExtension: ".html",
		log:               slog.Default().WithGroup("xtemplate"),
		config:            make(map[string]string),
		funcs:             make(template.FuncMap),
		files:             make(map[string]fileInfo),
		router:            http.NewServeMux(),
		ctx:               ctx,
		cancel:            cancel,
		id:                atomic.AddInt64(&instanceIdentity, 1),
	}

	// apply all configs, which are funcs that mutate the xtemplate struct
	for _, c := range *configs {
		c(x)
	}

	x.log = x.log.With(slog.Int64("instance", x.id))

	log := x.log.WithGroup("build")

	// Define the template instance that will accumulate all template definitions.
	x.templates = template.New(".").Delims(x.ldelim, x.rdelim).Funcs(x.funcs)

	// scan all files from the templatefs root
	if err := fs.WalkDir(x.templateFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ext := filepath.Ext(path); ext == x.templateExtension {
			err = x.addTemplateHandler(path, log)
			} else {
			err = x.addStaticFileHandler(path, log)
		}
		return err
	}); err != nil {
		return nil, fmt.Errorf("error scanning files: %v", err)
	}

	// Invoke all initilization templates, aka any template whose name starts with "INIT "
	for _, tmpl := range x.templates.Templates() {
		if strings.HasPrefix(tmpl.Name(), "INIT ") {
			context := &struct {
				baseContext
				fsContext
			}{
				baseContext{
					server: x,
					log:    log,
				},
				fsContext{
					fs: x.contextFS,
				},
			}
			err := tmpl.Execute(io.Discard, context)
			if err = context.resolvePendingTx(err); err != nil {
				return nil, fmt.Errorf("template initializer '%s' failed: %w", tmpl.Name(), err)
			}
		}
	}

	log.Debug("xtemplate instance initialized", slog.Any("xtemplate", x))
	return x, nil
}

func (server *xtemplate) addHandler(handlerPath string, handler Handler) {
	server.router.HandleFunc(handlerPath, server.mainHandler(handlerPath, handler))
}

type fileInfo struct {
	hash, contentType string
	encodings         []encodingInfo
}

type encodingInfo struct {
	encoding, path string
	size           int64
	modtime        time.Time
}

func (x *xtemplate) addStaticFileHandler(path_ string, log *slog.Logger) error {
	// Open and stat the file
	fsfile, err := x.templateFS.Open(path_)
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

	// Calculate the file hash. If there's a compressed file with the same
	// prefix, calculate the hash of the contents and check that they match.
	ext := filepath.Ext(path_)
	basepath := strings.TrimSuffix(path.Clean("/"+path_), ext)
	var sri string
	var reader io.Reader = fsfile
	var encoding string = "identity"
	file, exists := x.files[basepath]
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
		basepath = path.Clean("/" + path_)
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
		x.addHandler("GET "+basepath, serveFileHandler)
		log.Debug("added static file handler", slog.String("path", basepath), slog.String("filepath", path_), slog.String("contenttype", file.contentType), slog.Int64("size", size), slog.Time("modtime", stat.ModTime()), slog.String("hash", sri))
	} else {
		if file.hash != sri {
			return fmt.Errorf("encoded file contents did not match original file '%s': expected %s, got %s", path_, file.hash, sri)
		}
		file.encodings = append(file.encodings, encodingInfo{encoding: encoding, path: path_, size: size, modtime: stat.ModTime()})
		sort.Slice(file.encodings, func(i, j int) bool { return file.encodings[i].size < file.encodings[j].size })
		log.Debug("added static file encoding", slog.String("path", basepath), slog.String("filepath", path_), slog.String("encoding", encoding), slog.Int64("size", size), slog.Time("modtime", stat.ModTime()))
	}
	x.files[basepath] = file
	return nil
}

var routeMatcher *regexp.Regexp = regexp.MustCompile("^(GET|POST|PUT|PATCH|DELETE|SSE) (.*)$")

func (x *xtemplate) addTemplateHandler(path_ string, log *slog.Logger) error {
	content, err := fs.ReadFile(x.templateFS, path_)
	if err != nil {
		return fmt.Errorf("could not read template file '%s': %v", path_, err)
	}
	path_ = path.Clean("/" + path_)
	// parse each template file manually to have more control over its final
	// names in the template namespace.
	newtemplates, err := parse.Parse(path_, string(content), x.ldelim, x.rdelim, x.funcs, buliltinsSkeleton)
	if err != nil {
		return fmt.Errorf("could not parse template file '%s': %v", path_, err)
	}
	// add all templates
	for name, tree := range newtemplates {
		if x.templates.Lookup(name) != nil {
			log.Debug("overriding named template '%s' with definition from file: %s", name, path_)
		}
		tmpl, err := x.templates.AddParseTree(name, tree)
		if err != nil {
			return fmt.Errorf("could not add template '%s' from '%s': %v", name, path_, err)
		}
		if name == path_ {
			// don't register routes to hidden files
			_, file := filepath.Split(path_)
			if len(file) > 0 && file[0] == '.' {
				continue
			}
			// strip the extension from the handled path
			routePath := strings.TrimSuffix(path_, x.templateExtension)
			// files named 'index' handle requests to the directory
			if path.Base(routePath) == "index" {
				routePath = path.Dir(routePath)
			}
			if strings.HasSuffix(routePath, "/") {
				routePath += "{$}"
			}
			x.addHandler("GET "+routePath, bufferedTemplateHandler(tmpl))
			log.Debug("added path template handler", "method", "GET", "path", routePath, "template_path", path_)
		} else if matches := routeMatcher.FindStringSubmatch(name); len(matches) == 3 {
			method, path_ := matches[1], matches[2]
			if method == "SSE" {
				x.addHandler("GET "+path_, sseTemplateHandler(tmpl))
			} else {
				x.addHandler(method+" "+path_, bufferedTemplateHandler(tmpl))
			}
			log.Debug("added named template handler", "method", method, "path", path_, "template_name", name, "template_path", path_)
		}
	}
	return nil
}

var extensionContentTypes map[string]string = map[string]string{
	".css": "text/css; charset=utf-8",
	".js":  "text/javascript; charset=utf-8",
	".csv": "text/csv",
}
// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web

import (
	"bytes"
	"context"
	"crypto/sha512"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/divbast/pisati/internal/config"
)

// The conditions NewAssets's errors report, besides the context's own
// error and, for a file it could not read, that file's.
var (
	// ErrBuild is a front-end build that cannot be served: a manifest
	// that is not one Vite writes, or that names a file the build lacks,
	// a file outside it, one too big, one of a type Assets does not serve,
	// or an entry that would load files a page could not check.
	ErrBuild = errors.New("unusable front-end build")
	// ErrDevServer is dev mode with the Vite dev server or the base URL
	// away from localhost, 127.0.0.1 and [::1].
	ErrDevServer = errors.New("the Vite dev server is for development on localhost alone")
)

const (
	// manifestPath is where vp build writes its manifest in the build.
	manifestPath = ".vite/manifest.json"
	// staticPrefix is where Handler serves the build, and the base
	// vite.config.ts builds for.
	staticPrefix = "/static/"
	// immutable lets a browser keep a file for a year without asking
	// again: each file's name carries a hash of its content, so a changed
	// file has a new name.
	immutable = "public, max-age=31536000, immutable"
	// maxManifest and maxFile bound what NewAssets reads: far more than a
	// build of Pisati's front end writes.
	maxManifest = 1 << 20
	maxFile     = 32 << 20
)

// The front end's build, as `just fe-build` left internal/web/dist when
// the binary was compiled. all: keeps the manifest, which lives in .vite.
//
//go:embed all:dist
var dist embed.FS

// Dist returns the front end's build that the binary carries: at its root,
// .vite/manifest.json and the files it names, or, when nothing was built
// before the binary was, PLACEHOLDER alone.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic("web: fs.Sub refused dist, which is a valid path: " + err.Error())
	}
	return sub
}

// The front end's two entries, the inputs vite.config.ts builds, by their
// paths from the repository's root, which are also the keys Vite's
// manifest gives them.
const (
	// styleInput is the stylesheet every page links, a build of its own so
	// that a page with no script loads it.
	styleInput = "web/css/layers.css"
	// scriptInput is the script a page with something to enhance loads.
	scriptInput = "web/ts/main.ts"
)

// AssetKind is the element a page loads an asset with. The zero AssetKind
// is none, and only the zero AssetTag has it.
type AssetKind uint8

const (
	// Stylesheet is loaded with <link rel="stylesheet">.
	Stylesheet AssetKind = iota + 1
	// Script is loaded with <script type="module">, carrying the
	// response's nonce.
	Script
)

// String returns k as "stylesheet" or "script".
func (k AssetKind) String() string {
	switch k {
	case Stylesheet:
		return "stylesheet"
	case Script:
		return "script"
	}
	return "AssetKind(" + strconv.Itoa(int(k)) + ")"
}

// AssetTag is how a page loads one entry of the front end: the element's
// kind, the URL it loads, the integrity value it checks what it loads
// against, and whether it loads it in CORS mode. The zero AssetTag refers
// to nothing: its URL is empty.
type AssetTag struct {
	kind        AssetKind
	url         string
	integrity   string
	crossOrigin bool
}

// Kind returns the element the tag is: a stylesheet link or a module
// script.
func (t AssetTag) Kind() AssetKind {
	return t.kind
}

// URL returns what the element loads, for its href or src: a path under
// /static/, or, in dev mode, a URL on the Vite dev server.
func (t AssetTag) URL() string {
	return t.url
}

// Integrity returns the element's integrity attribute, sha384- and the
// base64 SHA-384 of the bytes Handler serves at URL, and whether it has
// one. In dev mode it has none: the dev server's files change as they are
// edited.
func (t AssetTag) Integrity() (string, bool) {
	return t.integrity, t.integrity != ""
}

// CrossOrigin reports whether the element needs crossorigin, as each one
// does in dev mode. The dev server is another origin, and a page whose
// responses are cross-origin isolated (Cross-Origin-Embedder-Policy:
// require-corp) loads another origin's file only in CORS mode.
func (t AssetTag) CrossOrigin() bool {
	return t.crossOrigin
}

// Assets is the front end as pages load it: the tags of its two entries,
// the stylesheet and the script, and the handler that serves its files.
// Outside dev mode both come from a build's manifest; in dev mode, every
// tag points at the Vite dev server. An Assets never changes once made,
// and is safe for concurrent use. The zero Assets has no tags and serves
// nothing, as for a build with no manifest.
type Assets struct {
	// style and script are the entries' tags: the zero AssetTag where
	// there is none.
	style, script AssetTag
	// files holds each file Handler serves, by its path in the build.
	files map[string]file
	// dev is the Vite dev server's origin in dev mode, and empty outside.
	dev string
}

// file is one file of the build, as Handler serves it.
type file struct {
	data        []byte
	contentType string
}

// NewAssets returns the front end for pages to load.
//
// Outside dev mode, when dev.ViteURL is unset, it reads the build fsys
// holds: Vite's manifest at .vite/manifest.json, and each file the manifest
// names. The two entries' tags give each one's file's URL under /static/
// and its integrity value, which NewAssets computes from the bytes it read and
// will serve, trusting nothing the manifest says about them. Only the
// files the manifest names are served, so neither the manifest nor
// anything else in fsys ever is. When fsys has no manifest, as before
// anything is built, there are no tags, pages render without the front
// end, and nothing is served. The manifest's other entries, if it has any,
// get no tag. Anything else the manifest gets wrong is an
// error wrapping ErrBuild: a page should never load a file that is not
// there, or one it cannot check.
//
// In dev mode, when dev.ViteURL is set, fsys is not read, and each entry's
// tag points at the Vite dev server's origin, with crossorigin and no
// integrity value; DevClient gives Vite's client script, and Handler
// serves nothing. Dev mode needs both the dev server and base on
// localhost, 127.0.0.1 or [::1], as config.Load ensures, and is otherwise
// an error wrapping ErrDevServer.
//
// It checks ctx before each file it reads, and returns ctx's error once
// ctx is done.
func NewAssets(ctx context.Context, fsys fs.FS, base config.BaseURL, dev config.Dev) (*Assets, error) {
	if vite, ok := dev.ViteOrigin(); ok {
		origin, err := devOrigin(base, vite)
		if err != nil {
			return nil, err
		}
		return &Assets{
			style:  AssetTag{kind: Stylesheet, url: origin + staticURL(styleInput), crossOrigin: true},
			script: AssetTag{kind: Script, url: origin + staticURL(scriptInput), crossOrigin: true},
			dev:    origin,
		}, nil
	}
	raw, err := readFile(ctx, fsys, manifestPath, maxManifest)
	if errors.Is(err, fs.ErrNotExist) {
		return &Assets{}, nil
	}
	if err != nil {
		return nil, err
	}
	m, err := parseManifest(raw)
	if err != nil {
		return nil, err
	}
	files := make(map[string]file)
	for _, name := range m.served() {
		f, err := readBuilt(ctx, fsys, name)
		if err != nil {
			return nil, err
		}
		files[name] = f
	}
	return &Assets{style: m.tag(styleInput, files), script: m.tag(scriptInput, files), files: files}, nil
}

// tag returns the tag of input, a build input, from files, which holds
// every file m names: the zero AssetTag when m has no such entry.
func (m manifest) tag(input string, files map[string]file) AssetTag {
	c, ok := m[input]
	if !ok || !c.IsEntry {
		return AssetTag{}
	}
	return AssetTag{kind: entryKind(c.File), url: staticURL(c.File), integrity: integrity(files[c.File].data)}
}

// StyleTag returns the tag of the stylesheet every page links, and
// whether there is one: outside dev mode when the build's manifest has the
// stylesheet's entry, and in dev mode always.
func (a *Assets) StyleTag() (AssetTag, bool) {
	return a.style, a.style.url != ""
}

// ScriptTag returns the tag of the script a page with something to
// enhance loads, and whether there is one: outside dev mode when the
// build's manifest has the script's entry, and in dev mode always.
func (a *Assets) ScriptTag() (AssetTag, bool) {
	return a.script, a.script.url != ""
}

// DevClient returns the tag of Vite's client, which keeps a page in step
// with the dev server as files are edited, and whether there is one: only
// in dev mode. A page loading it also needs <meta property="csp-nonce">
// carrying its response's nonce, which the client gives each style element
// it adds, since the policy allows them by that nonce alone.
func (a *Assets) DevClient() (AssetTag, bool) {
	if a.dev == "" {
		return AssetTag{}, false
	}
	return AssetTag{kind: Script, url: a.dev + staticURL("@vite/client"), crossOrigin: true}, true
}

// Handler serves the build's files, which requests name at /static/<path>,
// their full path, as the route GET /static/ passes them on: each with its
// own Content-Type, and with Cache-Control: public, max-age=31536000,
// immutable, since a file's name changes whenever its content does. It
// answers 404 to every other path, a directory's, the manifest's and every
// file the manifest does not name among them, with nothing cached; 405 to
// a method other than GET or HEAD; and ranges as http.ServeContent does.
// In dev mode, and with no manifest, it serves nothing.
func (a *Assets) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		name, ok := strings.CutPrefix(r.URL.Path, staticPrefix)
		f, found := a.files[name]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", f.contentType)
		h.Set("Cache-Control", immutable)
		// ServeContent drops Cache-Control from an error it answers, a
		// range it cannot satisfy, so no error is cached for a year. The
		// zero time sends no Last-Modified: the name says which version
		// this is.
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.data))
	})
}

// devOrigin returns the origin of dev's Vite dev server, for a site
// served at base, or an error wrapping ErrDevServer unless both are on
// localhost, 127.0.0.1 or [::1].
func devOrigin(base config.BaseURL, vite config.Origin) (string, error) {
	if !base.IsLocal() || !vite.IsLocal() {
		return "", fmt.Errorf("%w: the dev server %s and the base URL %q must both be on localhost, 127.0.0.1 or [::1]",
			ErrDevServer, vite.String(), base.String())
	}
	return vite.String(), nil
}

// manifest is Vite's build manifest, decoded strictly: each chunk the build
// wrote, by its source path, or, for a chunk with no source of its own, a
// name Vite gives it.
type manifest map[string]chunk

// chunk is one entry of the manifest, with every field Vite writes, as
// Vite's ManifestChunk declares them.
type chunk struct {
	File           string   `json:"file"`
	Src            string   `json:"src"`
	Name           string   `json:"name"`
	Names          []string `json:"names"`
	IsEntry        bool     `json:"isEntry"`
	IsDynamicEntry bool     `json:"isDynamicEntry"`
	Imports        []string `json:"imports"`
	DynamicImports []string `json:"dynamicImports"`
	CSS            []string `json:"css"`
	Assets         []string `json:"assets"`
}

// parseManifest decodes raw as Vite's manifest, refusing any field Vite's
// ManifestChunk lacks, and checks that every file it names can be served,
// a stylesheet or a script, and that no entry loads another file, which a
// page's tag could not check. Its errors wrap
// ErrBuild, and report every such problem at once.
func parseManifest(raw []byte) (manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrBuild, manifestPath, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %s holds more than one JSON value", ErrBuild, manifestPath)
	}
	if m == nil {
		return nil, fmt.Errorf("%w: %s is null, not an object", ErrBuild, manifestPath)
	}
	var errs []error
	for _, key := range slices.Sorted(maps.Keys(m)) {
		c := m[key]
		if c.IsEntry && len(c.Imports)+len(c.DynamicImports)+len(c.CSS) > 0 {
			errs = append(errs, fmt.Errorf("%w: entry %q imports other files, which its tag would load unchecked", ErrBuild, key))
		}
		for _, name := range c.files() {
			if err := servable(name); err != nil {
				errs = append(errs, fmt.Errorf("%w: %q names %q: %w", ErrBuild, key, name, err))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}

// files returns every file c names: its own, its stylesheets and its
// assets.
func (c chunk) files() []string {
	names := make([]string, 0, 1+len(c.CSS)+len(c.Assets))
	names = append(names, c.File)
	names = append(names, c.CSS...)
	return append(names, c.Assets...)
}

// served returns every file m names, each one once, in order.
func (m manifest) served() []string {
	var names []string
	for _, c := range m {
		names = append(names, c.files()...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// servable returns why name, a file the manifest names, cannot be served,
// or nil when it can.
func servable(name string) error {
	switch _, typed := contentType(name); {
	case !fs.ValidPath(name) || name == ".":
		return errors.New("it is not a clean path inside the build")
	case name == ".vite" || strings.HasPrefix(name, ".vite/"):
		return errors.New("it is the manifest's own directory")
	case !typed:
		return errors.New("Assets serves no file of its type")
	}
	return nil
}

// readBuilt returns the file name of the build, ready to serve.
func readBuilt(ctx context.Context, fsys fs.FS, name string) (file, error) {
	data, err := readFile(ctx, fsys, name, maxFile)
	if errors.Is(err, fs.ErrNotExist) {
		return file{}, fmt.Errorf("%w: the manifest names %q, which the build lacks: %w", ErrBuild, name, err)
	}
	if err != nil {
		return file{}, err
	}
	// servable has checked every name the manifest gives.
	t, _ := contentType(name)
	return file{data: data, contentType: t}, nil
}

// readFile returns the content of the file name in fsys, or an error
// wrapping ErrBuild when it is bigger than limit. It checks ctx first.
func readFile(ctx context.Context, fsys fs.FS, name string, limit int64) (data []byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, f.Close())
	}()
	data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %q is bigger than %d bytes", ErrBuild, name, limit)
	}
	return data, nil
}

// entryKind returns the kind of element that loads file, an entry's
// built file, which parseManifest has checked is a stylesheet or a script.
func entryKind(file string) AssetKind {
	if path.Ext(file) == ".css" {
		return Stylesheet
	}
	return Script
}

// contentType returns the Content-Type Handler serves name with, and
// whether it serves files of name's type at all: stylesheets and scripts,
// all a build of the front end writes. The types are fixed here, rather
// than read from the system's tables, so that a file is served the same
// way everywhere.
func contentType(name string) (string, bool) {
	switch path.Ext(name) {
	case ".css":
		return "text/css; charset=utf-8", true
	case ".js":
		return "text/javascript; charset=utf-8", true
	}
	return "", false
}

// staticURL returns the URL path Handler serves name, a path in the
// build, at.
func staticURL(name string) string {
	return (&url.URL{Path: staticPrefix + name}).EscapedPath()
}

// integrity returns data's integrity value: sha384- and its SHA-384 in
// base64, as a browser computes it for the integrity attribute.
func integrity(data []byte) string {
	sum := sha512.Sum384(data)
	return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
}

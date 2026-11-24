// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

package web_test

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/divbast/pisati/internal/config"
	"github.com/divbast/pisati/internal/web"
)

// The files of a build as vp build writes them, with the manifest it
// writes beside them.
const (
	stylesheet = "assets/layers-CzttEyLW.css"
	script     = "assets/main-BvRk9kiK.js"
	manifest   = `{
  "web/css/layers.css": {
    "file": "assets/layers-CzttEyLW.css",
    "name": "layers",
    "names": ["layers.css"],
    "src": "web/css/layers.css",
    "isEntry": true
  },
  "web/ts/main.ts": {
    "file": "assets/main-BvRk9kiK.js",
    "name": "main",
    "src": "web/ts/main.ts",
    "isEntry": true
  }
}`
	immutable = "public, max-age=31536000, immutable"
)

// build returns a build's files: manifest, the two files the manifest
// above names, a picture only some manifests name, an older build's
// script, a script beside the manifest, and the placeholder every
// checkout has.
func build(manifest string) fstest.MapFS {
	return fstest.MapFS{
		".vite/manifest.json":         {Data: []byte(manifest)},
		stylesheet:                    {Data: []byte("@layer reset{*{box-sizing:border-box}}")},
		script:                        {Data: []byte(`document.documentElement.dataset.theme="auto";`)},
		"assets/stale-Q1w2e3r4.js":    {Data: []byte("an older build's file")},
		".vite/stale-Q1w2e3r4.js":     {Data: []byte("not the build's")},
		"PLACEHOLDER":                 {Data: []byte("keeps go:embed compiling")},
		"assets/picture-Z9y8x7w6.svg": {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	}
}

// sri returns the integrity value of data, as a browser computes it.
func sri(data []byte) string {
	sum := sha512.Sum384(data)
	return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
}

// baseURL returns s as a base URL, failing the test when it is not one.
func baseURL(t *testing.T, s string) config.BaseURL {
	t.Helper()
	b, err := config.ParseBaseURL(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// devConfig returns the dev settings with vite as the Vite dev server,
// which, unlike config.Load, it lets be anywhere.
func devConfig(t *testing.T, vite string) config.Dev {
	t.Helper()
	u, err := config.ParseOptionalURL(vite)
	if err != nil {
		t.Fatal(err)
	}
	return config.Dev{ViteURL: u}
}

// production returns the Assets of fsys outside dev mode.
func production(t *testing.T, fsys fs.FS) *web.Assets {
	t.Helper()
	a, err := web.NewAssets(t.Context(), fsys, baseURL(t, "https://blog.example.org"), config.Dev{})
	if err != nil {
		t.Fatalf("NewAssets: %v", err)
	}
	return a
}

// get sends method for target through h, with each pair of header's
// strings as a request header.
func get(t testing.TB, h http.Handler, method, target string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// tags are the two ways a page asks for an entry's tag.
var tags = []struct {
	name string
	of   func(*web.Assets) (web.AssetTag, bool)
}{
	{name: "StyleTag", of: (*web.Assets).StyleTag},
	{name: "ScriptTag", of: (*web.Assets).ScriptTag},
}

func TestAssetsHashedImmutableWithSRI(t *testing.T) {
	fsys := build(manifest)
	a := production(t, fsys)
	for _, tt := range []struct {
		name              string
		of                func(*web.Assets) (web.AssetTag, bool)
		file, contentType string
		kind              web.AssetKind
	}{
		{name: "StyleTag", of: (*web.Assets).StyleTag, file: stylesheet, kind: web.Stylesheet, contentType: "text/css; charset=utf-8"},
		{name: "ScriptTag", of: (*web.Assets).ScriptTag, file: script, kind: web.Script, contentType: "text/javascript; charset=utf-8"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tag, ok := tt.of(a)
			if !ok {
				t.Fatalf("%s: no tag", tt.name)
			}
			if tag.Kind() != tt.kind {
				t.Errorf("Kind %v, want %v", tag.Kind(), tt.kind)
			}
			if want := "/static/" + tt.file; tag.URL() != want {
				t.Errorf("URL %q, want %q", tag.URL(), want)
			}
			integrity, ok := tag.Integrity()
			if want := sri(fsys[tt.file].Data); !ok || integrity != want {
				t.Errorf("Integrity %q, %v; want %q, computed from the file", integrity, ok, want)
			}
			if tag.CrossOrigin() {
				t.Error("CrossOrigin: a same-origin file needs no crossorigin")
			}

			w := get(t, a.Handler(), http.MethodGet, tag.URL())
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s: %d, want 200", tag.URL(), w.Code)
			}
			// What a browser checks: the integrity against the bytes it got.
			if got := sri(w.Body.Bytes()); got != integrity {
				t.Errorf("the body served hashes to %q, and the tag says %q", got, integrity)
			}
			for name, want := range map[string]string{"Cache-Control": immutable, "Content-Type": tt.contentType} {
				if got := w.Header().Get(name); got != want {
					t.Errorf("%s %q, want %q", name, got, want)
				}
			}

			w = get(t, a.Handler(), http.MethodHead, tag.URL())
			if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Cache-Control") != immutable {
				t.Errorf("HEAD %s: %d, %d bytes, Cache-Control %q; want 200, none, %q",
					tag.URL(), w.Code, w.Body.Len(), w.Header().Get("Cache-Control"), immutable)
			}
		})
	}

	if tag, ok := a.DevClient(); ok {
		t.Errorf("DevClient = %+v outside dev mode", tag)
	}
}

func TestAssetsServeOnlyBuiltFiles(t *testing.T) {
	a := production(t, build(manifest))
	h := a.Handler()

	for _, target := range []string{
		"/static/.vite/manifest.json",
		"/static/.vite/",
		"/static/.vite/stale-Q1w2e3r4.js",
		"/static/PLACEHOLDER",
		"/static/assets/stale-Q1w2e3r4.js",
		"/static/",
		"/static/assets/",
		"/static/assets",
		"/static/" + script + "/",
		"/" + script,
		"/static/assets/MAIN-BVRK9KIK.JS",
		"/static/assets/picture-Z9y8x7w6.svg",
	} {
		w := get(t, h, http.MethodGet, target)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", target, w.Code)
		}
		if got := w.Header().Get("Cache-Control"); strings.Contains(got, "immutable") {
			t.Errorf("GET %s: Cache-Control %q on a miss", target, got)
		}
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		w := get(t, h, method, "/static/"+script)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d, Allow %q; want 405, GET, HEAD", method, w.Code, w.Header().Get("Allow"))
		}
	}

	// A range past the end is refused, and the refusal is not cached.
	w := get(t, h, http.MethodGet, "/static/"+script, "Range", "bytes=9999-")
	if w.Code != http.StatusRequestedRangeNotSatisfiable || w.Header().Get("Cache-Control") != "" {
		t.Errorf("a range past the end: %d, Cache-Control %q; want 416 and none", w.Code, w.Header().Get("Cache-Control"))
	}
}

func TestAssetsAbsentManifestRendersWithout(t *testing.T) {
	for name, fsys := range map[string]fs.FS{
		"a fresh checkout": fstest.MapFS{"PLACEHOLDER": {Data: []byte("keeps go:embed compiling")}},
		"nothing at all":   fstest.MapFS{},
	} {
		t.Run(name, func(t *testing.T) {
			a := production(t, fsys)
			for _, tt := range tags {
				if tag, ok := tt.of(a); ok {
					t.Errorf("%s = %+v with no manifest", tt.name, tag)
				}
			}
			if w := get(t, a.Handler(), http.MethodGet, "/static/PLACEHOLDER"); w.Code != http.StatusNotFound {
				t.Errorf("GET /static/PLACEHOLDER: %d, want 404", w.Code)
			}
		})
	}
}

// TestDistHoldsThePlaceholder checks that the embedded build has the one
// file every checkout tracks, whether or not just fe-build has run.
func TestDistHoldsThePlaceholder(t *testing.T) {
	if _, err := fs.Stat(web.Dist(), "PLACEHOLDER"); err != nil {
		t.Fatalf("Dist: %v", err)
	}
}

func TestNewAssetsRefusesABadBuild(t *testing.T) {
	big := `{"web/ts/main.ts": {"file": "assets/main-BvRk9kiK.js", "isEntry": true, "name": "` +
		strings.Repeat("x", 1<<20) + `"}}`
	for _, tt := range []struct{ name, manifest string }{
		{name: "not JSON", manifest: `{"web/ts/main.ts": `},
		{name: "not an object", manifest: `["assets/main-BvRk9kiK.js"]`},
		{name: "a second value", manifest: manifest + `{}`},
		{name: "a field Vite never writes", manifest: `{"web/ts/main.ts": {"file": "assets/main-BvRk9kiK.js", "isEntry": true, "integrity": "sha384-x"}}`},
		{name: "a file not built", manifest: `{"web/ts/main.ts": {"file": "assets/main-00000000.js", "isEntry": true}}`},
		{name: "no file", manifest: `{"web/ts/main.ts": {"isEntry": true}}`},
		{name: "a file outside the build", manifest: `{"web/ts/main.ts": {"file": "../main.js", "isEntry": true}}`},
		{name: "an absolute file", manifest: `{"web/ts/main.ts": {"file": "/assets/main-BvRk9kiK.js", "isEntry": true}}`},
		{name: "the manifest itself", manifest: `{"m": {"file": ".vite/manifest.json"}}`},
		{name: "a script beside the manifest", manifest: `{"m": {"file": ".vite/stale-Q1w2e3r4.js"}}`},
		{name: "a file of no known type", manifest: `{"web/x.txt": {"file": "PLACEHOLDER"}}`},
		{name: "an entry that imports a chunk", manifest: `{"web/ts/main.ts": {"file": "assets/main-BvRk9kiK.js", "isEntry": true, "imports": ["_shared.js"]},
			"_shared.js": {"file": "assets/stale-Q1w2e3r4.js"}}`},
		{name: "an entry that imports later", manifest: `{"web/ts/main.ts": {"file": "assets/main-BvRk9kiK.js", "isEntry": true, "dynamicImports": ["_shared.js"]},
			"_shared.js": {"file": "assets/stale-Q1w2e3r4.js"}}`},
		{name: "an entry that imports a stylesheet", manifest: `{"web/ts/main.ts": {"file": "assets/main-BvRk9kiK.js", "isEntry": true, "css": ["assets/layers-CzttEyLW.css"]}}`},
		{name: "an asset of a type not served", manifest: `{"web/css/layers.css": {"file": "assets/layers-CzttEyLW.css", "isEntry": true, "assets": ["assets/picture-Z9y8x7w6.svg"]}}`},
		{name: "an asset not built", manifest: `{"web/css/layers.css": {"file": "assets/layers-CzttEyLW.css", "isEntry": true, "assets": ["assets/gone-00000000.svg"]}}`},
		{name: "a manifest over 1 MiB", manifest: big},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := web.NewAssets(t.Context(), build(tt.manifest), baseURL(t, "https://blog.example.org"), config.Dev{})
			if !errors.Is(err, web.ErrBuild) {
				t.Fatalf("NewAssets = %v, %v; want an error wrapping ErrBuild", a, err)
			}
		})
	}

	t.Run("a file over its limit", func(t *testing.T) {
		fsys := build(manifest)
		fsys[script] = &fstest.MapFile{Data: make([]byte, 32<<20+1)}
		if _, err := web.NewAssets(t.Context(), fsys, baseURL(t, "https://blog.example.org"), config.Dev{}); !errors.Is(err, web.ErrBuild) {
			t.Fatalf("NewAssets: %v, want an error wrapping ErrBuild", err)
		}
	})
}

func TestNewAssetsStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := web.NewAssets(ctx, build(manifest), baseURL(t, "https://blog.example.org"), config.Dev{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewAssets: %v, want context.Canceled", err)
	}
}

func TestDevModeOnlyOnLocalhost(t *testing.T) {
	for _, tt := range []struct{ name, base, vite string }{
		{name: "a public Vite server", base: "http://localhost:8080", vite: "http://vite.example.org:5173"},
		{name: "a private address", base: "http://localhost:8080", vite: "http://192.168.1.10:5173"},
		{name: "loopback, but not 127.0.0.1", base: "http://localhost:8080", vite: "http://127.0.0.2:5173"},
		{name: "a public site", base: "https://blog.example.org", vite: "http://localhost:5173"},
		{name: "a public https Vite server", base: "https://localhost:8443", vite: "https://vite.example.org"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := web.NewAssets(t.Context(), build(manifest), baseURL(t, tt.base), devConfig(t, tt.vite))
			if !errors.Is(err, web.ErrDevServer) {
				t.Fatalf("NewAssets = %v, %v; want an error wrapping ErrDevServer", a, err)
			}
		})
	}
	t.Run("no base URL", func(t *testing.T) {
		if _, err := web.NewAssets(t.Context(), build(manifest), config.BaseURL{}, devConfig(t, "http://localhost:5173")); !errors.Is(err, web.ErrDevServer) {
			t.Fatalf("NewAssets: %v, want an error wrapping ErrDevServer", err)
		}
	})

	for _, tt := range []struct{ base, vite, origin string }{
		{base: "http://localhost:8080", vite: "http://localhost:5173", origin: "http://localhost:5173"},
		{base: "http://127.0.0.1:8080", vite: "http://127.0.0.1:5173", origin: "http://127.0.0.1:5173"},
		{base: "http://[::1]:8080", vite: "http://[::1]:5173", origin: "http://[::1]:5173"},
		{base: "https://localhost:8443", vite: "https://localhost:5173/static/", origin: "https://localhost:5173"},
	} {
		t.Run(tt.vite, func(t *testing.T) {
			// The build's manifest is there, and dev mode passes it by.
			a, err := web.NewAssets(t.Context(), build(manifest), baseURL(t, tt.base), devConfig(t, tt.vite))
			if err != nil {
				t.Fatalf("NewAssets: %v", err)
			}
			for _, want := range []struct {
				name, input string
				of          func(*web.Assets) (web.AssetTag, bool)
				kind        web.AssetKind
			}{
				{name: "ScriptTag", input: "web/ts/main.ts", of: (*web.Assets).ScriptTag, kind: web.Script},
				{name: "StyleTag", input: "web/css/layers.css", of: (*web.Assets).StyleTag, kind: web.Stylesheet},
			} {
				tag, ok := want.of(a)
				if !ok {
					t.Fatalf("%s: none in dev mode", want.name)
				}
				if u := tt.origin + "/static/" + want.input; tag.URL() != u {
					t.Errorf("URL %q, want %q", tag.URL(), u)
				}
				if tag.Kind() != want.kind {
					t.Errorf("Kind %v, want %v", tag.Kind(), want.kind)
				}
				// The dev server's files change as they are edited.
				if integrity, ok := tag.Integrity(); ok {
					t.Errorf("Integrity %q in dev mode", integrity)
				}
				if !tag.CrossOrigin() {
					t.Error("CrossOrigin false: an isolated page loads the dev server's files only with CORS")
				}
			}
			client, ok := a.DevClient()
			if !ok || client.URL() != tt.origin+"/static/@vite/client" || client.Kind() != web.Script || !client.CrossOrigin() {
				t.Errorf("DevClient = %+v, %v; want the crossorigin script %s/static/@vite/client", client, ok, tt.origin)
			}
			if _, ok := client.Integrity(); ok {
				t.Error("DevClient has an integrity value")
			}
			// Vite serves every file; Pisati serves none of the build's.
			if w := get(t, a.Handler(), http.MethodGet, "/static/"+script); w.Code != http.StatusNotFound {
				t.Errorf("GET /static/%s in dev mode: %d, want 404", script, w.Code)
			}
		})
	}
}

// viteInputs returns the inputs vite.config.ts builds.
func viteInputs(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("../../vite.config.ts")
	if err != nil {
		t.Fatal(err)
	}
	list := regexp.MustCompile(`(?s)\binput:\s*\[(.*?)\]`).FindSubmatch(source)
	if list == nil {
		t.Fatal("vite.config.ts names no input list")
	}
	var inputs []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(list[1], -1) {
		inputs = append(inputs, string(m[1]))
	}
	return inputs
}

// TestTagsAreViteInputs checks that StyleTag and ScriptTag answer for
// exactly the inputs vite.config.ts builds: a stylesheet and a script, by
// the keys Vite's manifest gives them.
func TestTagsAreViteInputs(t *testing.T) {
	inputs := viteInputs(t)
	if len(inputs) != 2 {
		t.Fatalf("vite.config.ts builds %q, and Assets has tags for two inputs", inputs)
	}
	files := map[string]string{".css": stylesheet, ".js": script, ".ts": script}
	var b strings.Builder
	b.WriteString("{")
	for i, input := range inputs {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q: {%q: %q, %q: true}", input, "file", files[path.Ext(input)], "isEntry")
	}
	b.WriteString("}")
	a := production(t, build(b.String()))
	for _, tt := range tags {
		if _, ok := tt.of(a); !ok {
			t.Errorf("%s: none for a manifest of vite.config.ts's inputs %q", tt.name, inputs)
		}
	}
}

// TestTagsOnlyForTheBuildInputs checks that no other entry of the
// manifest, nor a chunk keyed by an input that is no entry, gets a tag.
func TestTagsOnlyForTheBuildInputs(t *testing.T) {
	for name, m := range map[string]string{
		"other entries": `{"web/css/other.css": {"file": "assets/layers-CzttEyLW.css", "isEntry": true},
			"web/ts/other.ts": {"file": "assets/main-BvRk9kiK.js", "isEntry": true}}`,
		"the inputs, as chunks alone": `{"web/css/layers.css": {"file": "assets/layers-CzttEyLW.css"},
			"web/ts/main.ts": {"file": "assets/main-BvRk9kiK.js"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			a := production(t, build(m))
			for _, tt := range tags {
				if tag, ok := tt.of(a); ok {
					t.Errorf("%s = %+v, want none", tt.name, tag)
				}
			}
		})
	}
}

func TestAssetKindString(t *testing.T) {
	for kind, want := range map[web.AssetKind]string{
		web.Stylesheet:    "stylesheet",
		web.Script:        "script",
		web.AssetKind(0):  "AssetKind(0)",
		web.AssetKind(99): "AssetKind(99)",
	} {
		if got := kind.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", kind, got, want)
		}
	}
}

func TestZeroAssetTag(t *testing.T) {
	var tag web.AssetTag
	if _, ok := tag.Integrity(); ok || tag.URL() != "" || tag.Kind() != 0 || tag.CrossOrigin() {
		t.Errorf("the zero AssetTag = %+v, want one that refers to nothing", tag)
	}
}

// FuzzNewAssets checks that every manifest gives either an error wrapping
// ErrBuild or Assets whose every tag names a file the handler serves, with
// the integrity of what it serves, and that the handler never serves the
// manifest.
func FuzzNewAssets(f *testing.F) {
	f.Add(manifest)
	f.Add(`{"web/ts/main.ts": {"file": "assets/main-BvRk9kiK.js", "isEntry": true}}`)
	f.Add(`{"web/css/layers.css": {"file": "assets/layers-CzttEyLW.css", "isEntry": true, "assets": ["assets/stale-Q1w2e3r4.js"]}}`)
	f.Add(`{"a": {"file": ".vite/manifest.json"}}`)
	f.Add(`{"a": {"file": "assets/stale-Q1w2e3r4.js", "imports": ["b"]}, "b": {"file": "PLACEHOLDER"}}`)
	f.Fuzz(func(t *testing.T, m string) {
		a, err := web.NewAssets(t.Context(), build(m), baseURL(t, "https://blog.example.org"), config.Dev{})
		if err != nil {
			if !errors.Is(err, web.ErrBuild) {
				t.Fatalf("NewAssets: %v, which does not wrap ErrBuild", err)
			}
			return
		}
		h := a.Handler()
		for _, target := range []string{"/static/.vite/manifest.json", "/static/PLACEHOLDER", "/static/"} {
			if w := get(t, h, http.MethodGet, target); w.Code != http.StatusNotFound {
				t.Fatalf("GET %s: %d", target, w.Code)
			}
		}
		for _, tt := range tags {
			tag, ok := tt.of(a)
			if !ok {
				continue
			}
			integrity, ok := tag.Integrity()
			if !ok || (tag.Kind() != web.Script && tag.Kind() != web.Stylesheet) {
				t.Fatalf("%s = %+v", tt.name, tag)
			}
			w := get(t, h, http.MethodGet, tag.URL())
			if w.Code != http.StatusOK || sri(w.Body.Bytes()) != integrity {
				t.Fatalf("GET %s for %s: %d, the body hashing to %s, not %s", tag.URL(), tt.name, w.Code, sri(w.Body.Bytes()), integrity)
			}
		}
	})
}

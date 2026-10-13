/*
	Copyright 2026 NetFoundry Inc.

	Licensed under the Apache License, Version 2.0 (the "License");
	you may not use this file except in compliance with the License.
	You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

	Unless required by applicable law or agreed to in writing, software
	distributed under the License is distributed on an "AS IS" BASIS,
	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
	See the License for the specific language governing permissions and
	limitations under the License.
*/

package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	gmutil "github.com/yuin/goldmark/util"

	"github.com/netfoundry/ziti-stitch/docs"
)

// The guides in docs/, served from the binary.

const docsPath = "/docs/"

// docPage is one guide, titled by its first heading.
type docPage struct {
	Name  string // "install"
	Title string
	HTML  template.HTML
}

var (
	docsOnce   sync.Once
	docsByName map[string]*docPage
	docsSorted []*docPage
	docsErr    error
)

// loadDocs renders every guide once. They cannot change without a new
// binary.
func loadDocs() (map[string]*docPage, []*docPage, error) {
	docsOnce.Do(func() {
		names, err := fs.Glob(docs.FS, "*.md")
		if err != nil {
			docsErr = err
			return
		}
		published := map[string]bool{}
		for _, n := range names {
			published[n] = true
		}
		md := goldmark.New(goldmark.WithExtensions(extension.Table), goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
			parser.WithASTTransformers(gmutil.Prioritized(docLinks{published}, 100)),
		))
		docsByName = map[string]*docPage{}
		for _, n := range names {
			src, err := docs.FS.ReadFile(n)
			if err != nil {
				docsErr = err
				return
			}
			var buf bytes.Buffer
			if err := md.Convert(src, &buf); err != nil {
				docsErr = err
				return
			}
			// The guides are ours, compiled in, and goldmark escapes raw
			// HTML by default.
			p := &docPage{Name: strings.TrimSuffix(n, ".md"), Title: firstHeading(src), HTML: template.HTML(buf.String())}
			docsByName[p.Name] = p
			docsSorted = append(docsSorted, p)
		}
		slices.SortFunc(docsSorted, func(a, b *docPage) int { return strings.Compare(a.Title, b.Title) })
	})
	return docsByName, docsSorted, docsErr
}

func firstHeading(src []byte) string {
	for _, line := range strings.Split(string(src), "\n") {
		if t, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(t)
		}
	}
	return ""
}

// docLinks points links between guides at their pages here. A link to a
// file that is not a published guide keeps its words and loses its href.
type docLinks struct{ published map[string]bool }

func (d docLinks) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	var links []*ast.Link
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if l, ok := n.(*ast.Link); ok && entering {
			links = append(links, l)
		}
		return ast.WalkContinue, nil
	})
	// Collected first: the walk is not a safe place to reparent nodes.
	for _, l := range links {
		file, frag, _ := strings.Cut(string(l.Destination), "#")
		if !strings.HasSuffix(file, ".md") || strings.Contains(file, "/") {
			continue
		}
		if d.published[file] {
			to := docsPath + strings.TrimSuffix(file, ".md")
			if frag != "" {
				to += "#" + frag
			}
			l.Destination = []byte(to)
			continue
		}
		parent := l.Parent()
		if parent == nil {
			continue
		}
		for c := l.FirstChild(); c != nil; {
			next := c.NextSibling()
			parent.InsertBefore(parent, l, c)
			c = next
		}
		parent.RemoveChild(parent, l)
	}
}

type docsData struct {
	Doc  *docPage
	Docs []*docPage
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request, v *view) {
	byName, sorted, err := loadDocs()
	if err != nil {
		s.renderError(w, r, v, http.StatusInternalServerError, err.Error())
		return
	}
	v.Nav = "docs"
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, docsPath), "/")
	if name == "" {
		v.Title, v.Data = "Documentation", docsData{Docs: sorted}
		s.render(w, http.StatusOK, "docs_index", "", v)
		return
	}
	p, ok := byName[name]
	if !ok {
		s.renderError(w, r, v, http.StatusNotFound, "There is no such guide.")
		return
	}
	v.Title, v.Data = p.Title, docsData{Doc: p, Docs: sorted}
	s.render(w, http.StatusOK, "docs_page", "", v)
}

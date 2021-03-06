// Copyright 2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package v4

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/juju/jujusvg"
	"github.com/juju/xml"
	"gopkg.in/errgo.v1"
	"gopkg.in/juju/charm.v4"

	"github.com/juju/charmstore/internal/mongodoc"
	"github.com/juju/charmstore/internal/router"
	"github.com/juju/charmstore/params"
)

// GET id/diagram.svg
// http://tinyurl.com/nqjvxov
func (h *Handler) serveDiagram(id *charm.Reference, fullySpecified bool, w http.ResponseWriter, req *http.Request) error {
	if id.Series != "bundle" {
		return errgo.WithCausef(nil, params.ErrNotFound, "diagrams not supported for charms")
	}
	entity, err := h.store.FindEntity(id, "bundledata")
	if err != nil {
		return errgo.Mask(err, errgo.Is(params.ErrNotFound))
	}

	var urlErr error
	// TODO consider what happens when a charm's SVG does not exist.
	canvas, err := jujusvg.NewFromBundle(entity.BundleData, func(id *charm.Reference) string {
		// TODO change jujusvg so that the iconURL function can
		// return an error.
		absPath := "/" + id.Path() + "/icon.svg"
		p, err := router.RelativeURLPath(req.RequestURI, absPath)
		if err != nil {
			urlErr = errgo.Notef(err, "cannot make relative URL from %q and %q", req.RequestURI, absPath)
		}
		return p
	})
	if err != nil {
		return errgo.Notef(err, "cannot create canvas")
	}
	if urlErr != nil {
		return urlErr
	}
	setArchiveCacheControl(w.Header(), fullySpecified)
	w.Header().Set("Content-Type", "image/svg+xml")
	canvas.Marshal(w)
	return nil
}

// These are all forms of README files
// actually observed in charms in the wild.
var allowedReadMe = map[string]bool{
	"readme":          true,
	"readme.md":       true,
	"readme.rst":      true,
	"readme.ex":       true,
	"readme.markdown": true,
	"readme.txt":      true,
}

// GET id/readme
// http://tinyurl.com/kygyvot
func (h *Handler) serveReadMe(id *charm.Reference, fullySpecified bool, w http.ResponseWriter, req *http.Request) error {
	entity, err := h.store.FindEntity(id, "_id", "contents", "blobname")
	if err != nil {
		return errgo.NoteMask(err, "cannot get README", errgo.Is(params.ErrNotFound))
	}
	isReadMeFile := func(f *zip.File) bool {
		name := strings.ToLower(path.Clean(f.Name))
		// This is the same condition currently used by the GUI.
		// TODO propagate likely content type from file extension.
		return allowedReadMe[name]
	}
	r, err := h.store.OpenCachedBlobFile(entity, mongodoc.FileReadMe, isReadMeFile)
	if err != nil {
		return errgo.Mask(err, errgo.Is(params.ErrNotFound))
	}
	defer r.Close()
	setArchiveCacheControl(w.Header(), fullySpecified)
	io.Copy(w, r)
	return nil
}

// GET id/icon.svg
// http://tinyurl.com/lhodocb
func (h *Handler) serveIcon(id *charm.Reference, fullySpecified bool, w http.ResponseWriter, req *http.Request) error {
	if id.Series == "bundle" {
		return errgo.WithCausef(nil, params.ErrNotFound, "icons not supported for bundles")
	}

	entity, err := h.store.FindEntity(id, "_id", "contents", "blobname")
	if err != nil {
		return errgo.NoteMask(err, "cannot get icon", errgo.Is(params.ErrNotFound))
	}
	isIconFile := func(f *zip.File) bool {
		return path.Clean(f.Name) == "icon.svg"
	}
	r, err := h.store.OpenCachedBlobFile(entity, mongodoc.FileIcon, isIconFile)
	if err != nil {
		if errgo.Cause(err) != params.ErrNotFound {
			return errgo.Mask(err)
		}
		setArchiveCacheControl(w.Header(), fullySpecified)
		w.Header().Set("Content-Type", "image/svg+xml")
		io.Copy(w, strings.NewReader(defaultIcon))
		return nil
	}
	defer r.Close()
	w.Header().Set("Content-Type", "image/svg+xml")
	setArchiveCacheControl(w.Header(), fullySpecified)
	if err := processIcon(w, r); err != nil {
		return errgo.Mask(err)
	}
	return nil
}

const svgNamespace = "http://www.w3.org/2000/svg"

// processIcon reads an icon SVG from r and writes
// it to w, making any changes that need to be made.
// Currently it adds a viewBox attribute to the <svg>
// element if necessary.
func processIcon(w io.Writer, r io.Reader) error {
	dec := xml.NewDecoder(r)
	dec.DefaultSpace = svgNamespace
	enc := xml.NewEncoder(w)
	ensured := false
	// This could be slightly more efficient, because
	// for large icons which already have a viewbox,
	// we are doing more marshaling/unmarshaling work
	// than we need to. But icons will be cached, and the
	// extra overhead isn't that big, so we go with the
	// simple approach, trusting that the icon still works
	// after being processed through the xml package.
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read token: %v", err)
		}
		if !ensured {
			tok, ensured = ensureViewbox(tok)
		}
		if err := enc.EncodeToken(tok); err != nil {
			return fmt.Errorf("cannot encode token %#v: %v", tok, err)
		}
	}
	if err := enc.Flush(); err != nil {
		return fmt.Errorf("cannot flush output: %v", err)
	}
	return nil
}

func ensureViewbox(tok0 xml.Token) (_ xml.Token, found bool) {
	tok, ok := tok0.(xml.StartElement)
	if !ok || tok.Name.Space != svgNamespace || tok.Name.Local != "svg" {
		return tok0, false
	}
	var width, height string
	for _, attr := range tok.Attr {
		if attr.Name.Space != "" {
			continue
		}
		switch attr.Name.Local {
		case "width":
			width = attr.Value
		case "height":
			height = attr.Value
		case "viewBox":
			return tok, true
		}
	}
	if width == "" || height == "" {
		// Width and/or height have not been specified,
		// so leave viewbox unspecified too.
		return tok, true
	}
	tok.Attr = append(tok.Attr, xml.Attr{
		Name: xml.Name{
			Local: "viewBox",
		},
		Value: fmt.Sprintf("0 0 %s %s", width, height),
	})
	return tok, true
}

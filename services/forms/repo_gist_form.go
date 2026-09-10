// Copyright 2025 The cgit instance. All rights reserved.
// SPDX-License-Identifier: MIT

package forms

import (
	"net/http"

	"gitea.dev/modules/web/middleware"
	"gitea.dev/services/context"

	"gitea.com/go-chi/binding"
)

// NewGistForm form for creating a gist directly on this instance (cgit
// addition — see routers/web/repo/gist.go's GistNewPost). Deliberately
// small next to CreateRepoForm: a gist is one file, owned by the current
// user, with no owner/template/gitignore/license choices to make.
type NewGistForm struct {
	Description string `binding:"MaxSize(2048)"`
	FileName    string `binding:"Required;MaxSize(255)"`
	// MaxSize(1048576): 1MiB is already far past what a "gist" (a text
	// snippet) should be — generous headroom without letting the form
	// become a general-purpose large-file upload path.
	Content string `binding:"Required;MaxSize(1048576)"`
	Private bool
}

// Validate wires this form into the same Err_<Field> flagging that every
// other Gitea form template relies on (e.g. {{if .Err_FileName}}error{{end}}).
func (f *NewGistForm) Validate(req *http.Request, errs binding.Errors) binding.Errors {
	ctx := context.GetValidateContext(req)
	return middleware.Validate(errs, ctx.Data, f, ctx.Locale)
}

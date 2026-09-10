// Copyright 2025 The cgit instance. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"fmt"
	"net/http"
	"strings"

	repo_model "gitea.dev/models/repo"
	"gitea.dev/modules/log"
	"gitea.dev/modules/templates"
	"gitea.dev/modules/util"
	"gitea.dev/modules/web"
	"gitea.dev/services/context"
	"gitea.dev/services/forms"
	repo_service "gitea.dev/services/repository"
	files_service "gitea.dev/services/repository/files"
)

// cgit addition: native gist creation (no upstream equivalent). Companion
// to gitea-patches/0001 and 0002's Gists tab — those only *display* repos
// mirrored in from GitHub by migrate_gists.sh; this lets you create one
// directly on this instance instead, with no GitHub round trip. See
// custom-ui/templates/repo/gist_new.tmpl for the form and
// routers/web/web.go's "/repo/gist/new" routes for the wiring.
const tplGistNew templates.TplName = "repo/gist_new"

// gistLocalNamePrefix marks a gist repo as created natively here rather
// than mirrored from GitHub. migrate_gists.sh names its mirrors
// "gist-<github-id>", and GitHub gist ids are bare lowercase hex — the
// literal "local-" in this prefix can never appear in one, so a native
// gist's name can never collide with (or later get mistaken for) a
// mirrored one, while still starting with "gist-" so it's picked up by the
// same NamePrefix filtering that puts mirrors on the Gists tab.
const gistLocalNamePrefix = "gist-local-"

// GistNew renders the "New Gist" form.
func GistNew(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("new_gist")
	ctx.Data["PageIsGistNew"] = true
	ctx.Data["private"] = true // default to private, matching migrate_gists.sh's mirrored gists
	ctx.HTML(http.StatusOK, tplGistNew)
}

// GistNewPost creates a private-by-default, single-file repo directly on
// this instance: an empty repo (repo_service.CreateRepository with
// AutoInit: false), then one commit adding the submitted file
// (files_service.ChangeRepoFiles) — the same underlying call the web file
// editor uses to seed the very first commit of an empty repo.
func GistNewPost(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.NewGistForm)
	ctx.Data["Title"] = ctx.Tr("new_gist")
	ctx.Data["PageIsGistNew"] = true

	if ctx.HasError() {
		ctx.HTML(http.StatusOK, tplGistNew)
		return
	}

	fileName := files_service.CleanGitTreePath(strings.TrimSpace(form.FileName))
	if fileName == "" {
		ctx.RenderWithErrDeprecated(ctx.Tr("repo.editor.filename_cannot_be_empty"), tplGistNew, &form)
		return
	}

	repoName := gistLocalNamePrefix + strings.ToLower(util.CryptoRandomString(12))

	repo, err := repo_service.CreateRepository(ctx, ctx.Doer, ctx.Doer, repo_service.CreateRepoOptions{
		Name:             repoName,
		Description:      form.Description,
		IsPrivate:        form.Private,
		DefaultBranch:    "main",
		AutoInit:         false,
		TrustModel:       repo_model.DefaultTrustModel,
		ObjectFormatName: "sha1",
	})
	if err != nil {
		log.Error("GistNewPost: CreateRepository: %v", err)
		ctx.RenderWithErrDeprecated(ctx.Tr("repo.gists.create_failed", err.Error()), tplGistNew, &form)
		return
	}

	if _, err := files_service.ChangeRepoFiles(ctx, repo, ctx.Doer, &files_service.ChangeRepoFilesOptions{
		Message: fmt.Sprintf("Add %s", fileName),
		Files: []*files_service.ChangeRepoFile{
			{
				Operation:     "create",
				TreePath:      fileName,
				ContentReader: strings.NewReader(form.Content),
			},
		},
	}); err != nil {
		// The repo exists but is still empty — surface the error instead of
		// losing the user's typed content silently, and send them to the
		// (empty) repo rather than re-showing the form, since retrying here
		// would try to create a second repo rather than retry the commit.
		log.Error("GistNewPost: ChangeRepoFiles: %v", err)
		ctx.Flash.Error(ctx.Tr("repo.gists.create_file_failed", err.Error()))
		ctx.Redirect(repo.Link())
		return
	}

	ctx.Redirect(repo.Link())
}

// Copyright 2015 The Gogs Authors. All rights reserved.
// Copyright 2019 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package user

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"

	activities_model "gitea.dev/models/activities"
	"gitea.dev/models/db"
	"gitea.dev/models/organization"
	"gitea.dev/models/renderhelper"
	repo_model "gitea.dev/models/repo"
	user_model "gitea.dev/models/user"
	"gitea.dev/modules/git"
	"gitea.dev/modules/gitrepo"
	"gitea.dev/modules/highlight"
	"gitea.dev/modules/log"
	"gitea.dev/modules/markup/markdown"
	"gitea.dev/modules/optional"
	"gitea.dev/modules/setting"
	"gitea.dev/modules/templates"
	"gitea.dev/modules/typesniffer"
	"gitea.dev/routers/web/feed"
	"gitea.dev/routers/web/org"
	shared_user "gitea.dev/routers/web/shared/user"
	"gitea.dev/services/context"
	feed_service "gitea.dev/services/feed"
)

const (
	tplProfileBigAvatar templates.TplName = "shared/user/profile_big_avatar"
	tplFollowUnfollow   templates.TplName = "org/follow_unfollow"
)

// OwnerProfile render profile page for a user or a organization (aka, repo owner)
func OwnerProfile(ctx *context.Context) {
	if strings.Contains(ctx.Req.Header.Get("Accept"), "application/rss+xml") {
		feed.ShowUserFeedRSS(ctx)
		return
	}
	if strings.Contains(ctx.Req.Header.Get("Accept"), "application/atom+xml") {
		feed.ShowUserFeedAtom(ctx)
		return
	}

	if ctx.ContextUser.IsOrganization() {
		org.Home(ctx)
	} else {
		userProfile(ctx)
	}
}

func userProfile(ctx *context.Context) {
	// check view permissions
	if !user_model.IsUserVisibleToViewer(ctx, ctx.ContextUser, ctx.Doer) {
		ctx.NotFound(fmt.Errorf("%s", ctx.ContextUser.Name))
		return
	}

	ctx.Data["Title"] = ctx.ContextUser.DisplayName()
	ctx.Data["PageIsUserProfile"] = true

	profileDbRepo, profileReadmeBlob := shared_user.FindOwnerProfileReadme(ctx, ctx.Doer)

	prepareUserProfileTabData(ctx, profileDbRepo, profileReadmeBlob)

	// prepare the user nav header data after "prepareUserProfileTabData" to avoid re-querying the NumFollowers & NumFollowing
	// because ctx.Data["NumFollowers"] and "NumFollowing" logic duplicates in both of them
	// and the "profile readme" related logic also duplicates in both of FindOwnerProfileReadme and RenderUserOrgHeader
	// TODO: it is a bad design and should be refactored later,
	if _, err := shared_user.RenderUserOrgHeader(ctx); err != nil {
		ctx.ServerError("RenderUserOrgHeader", err)
		return
	}
	ctx.HTML(http.StatusOK, tplProfile)
}

// cgit addition: powers the file-content preview shown on each card of the
// profile "Gists" tab (see templates/shared/repo/gist_list.tmpl), so a gist
// there looks like an actual gist — a snippet you can read at a glance — not
// just a bare repo-list entry with a name and a star count.
const (
	// gistPreviewByteLimit caps how much of a gist's primary file is read
	// off disk for the preview card — enough for a typical snippet without
	// pulling a multi-MB file into a listing page that renders several of
	// these per request.
	gistPreviewByteLimit = 8 * 1024
	// gistPreviewLineLimit caps how many lines the card actually displays,
	// independent of the byte cap above: a file with very long lines could
	// stay under gistPreviewByteLimit while still being unreadable as a
	// preview.
	gistPreviewLineLimit = 20
)

// GistPreview is the per-repo data the Gists tab template renders inside
// each card. IsEmpty/IsBinary short-circuit to a plain notice instead of a
// broken or garbled code block.
type GistPreview struct {
	FileName  string
	FileCount int
	Content   template.HTML
	Truncated bool
	IsEmpty   bool
	IsBinary  bool
}

// buildGistPreviews renders one GistPreview per repo, keyed by repo ID
// (matching how the template looks them up via `index $.GistPreviews .ID`).
// Best-effort per repo: a repo that fails to open or read just gets
// IsEmpty=true rather than failing the whole profile page.
func buildGistPreviews(ctx *context.Context, repos []*repo_model.Repository) map[int64]*GistPreview {
	previews := make(map[int64]*GistPreview, len(repos))
	for _, repo := range repos {
		previews[repo.ID] = buildGistPreview(ctx, repo)
	}
	return previews
}

func buildGistPreview(ctx *context.Context, repo *repo_model.Repository) *GistPreview {
	preview := &GistPreview{}

	gitRepo, err := gitrepo.OpenRepository(ctx, repo)
	if err != nil {
		log.Error("buildGistPreview: OpenRepository %s: %v", repo.FullName(), err)
		preview.IsEmpty = true
		return preview
	}
	defer gitRepo.Close()

	// A freshly-migrated mirror that hasn't finished its first sync yet has
	// no commits on its default branch — treat that the same as an empty
	// gist rather than erroring the whole page.
	commit, err := gitRepo.GetBranchCommit(repo.DefaultBranch)
	if err != nil {
		preview.IsEmpty = true
		return preview
	}

	entries, err := commit.ListEntries()
	if err != nil {
		log.Error("buildGistPreview: ListEntries %s: %v", repo.FullName(), err)
		preview.IsEmpty = true
		return preview
	}

	files := make(git.Entries, 0, len(entries))
	for _, e := range entries {
		if e.IsRegular() || e.IsExecutable() {
			files = append(files, e)
		}
	}
	if len(files) == 0 {
		preview.IsEmpty = true
		return preview
	}
	// Deterministic pick of the "primary" file: a real gist is usually a
	// single file, but nothing stops it from having more — same as GitHub,
	// which shows one file per gist page and links to the rest.
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	primary := files[0]

	preview.FileName = primary.Name()
	preview.FileCount = len(files)

	raw, err := primary.Blob().GetBlobBytes(gistPreviewByteLimit)
	if err != nil {
		log.Error("buildGistPreview: GetBlobBytes %s/%s: %v", repo.FullName(), primary.Name(), err)
		preview.IsEmpty = true
		return preview
	}
	if len(raw) == 0 {
		preview.IsEmpty = true
		return preview
	}
	if !typesniffer.DetectContentType(raw).IsRepresentableAsText() {
		preview.IsBinary = true
		return preview
	}

	text := string(raw)
	lines := strings.Split(text, "\n")
	if len(lines) > gistPreviewLineLimit {
		text = strings.Join(lines[:gistPreviewLineLimit], "\n")
		preview.Truncated = true
	}
	if primary.Size() > gistPreviewByteLimit {
		preview.Truncated = true
	}

	preview.Content, _, _ = highlight.RenderCodeSlowGuess(primary.Name(), "", text)
	return preview
}

func prepareUserProfileTabData(ctx *context.Context, profileDbRepo *repo_model.Repository, profileReadme *git.Blob) {
	// if there is a profile readme, default to "overview" page, otherwise, default to "repositories" page
	// if there is not a profile readme, the overview tab should be treated as the repositories tab
	tab := ctx.FormString("tab")
	if tab == "" || tab == "overview" {
		if profileReadme != nil {
			tab = "overview"
		} else {
			tab = "repositories"
		}
	}
	ctx.Data["TabName"] = tab
	ctx.Data["HasUserProfileReadme"] = profileReadme != nil

	page := ctx.FormInt("page")
	if page <= 0 {
		page = 1
	}

	pagingNum := setting.UI.User.RepoPagingNum
	topicOnly := ctx.FormBool("topic")
	var (
		repos   []*repo_model.Repository
		count   int64
		total   int64
		curRows int
		orderBy db.SearchOrderBy
	)

	sortOrder := ctx.FormString("sort")
	if _, ok := repo_model.OrderByFlatMap[sortOrder]; !ok {
		sortOrder = setting.UI.ExploreDefaultSort // TODO: add new default sort order for user home?
	}
	ctx.Data["SortType"] = sortOrder
	orderBy = repo_model.OrderByFlatMap[sortOrder]

	keyword := ctx.FormTrim("q")
	ctx.Data["Keyword"] = keyword

	language := ctx.FormTrim("language")
	ctx.Data["Language"] = language

	followers, numFollowers, err := user_model.GetUserFollowers(ctx, ctx.ContextUser, ctx.Doer, db.ListOptions{
		PageSize: pagingNum,
		Page:     page,
	})
	if err != nil {
		ctx.ServerError("GetUserFollowers", err)
		return
	}
	ctx.Data["NumFollowers"] = numFollowers
	following, numFollowing, err := user_model.GetUserFollowing(ctx, ctx.ContextUser, ctx.Doer, db.ListOptions{
		PageSize: pagingNum,
		Page:     page,
	})
	if err != nil {
		ctx.ServerError("GetUserFollowing", err)
		return
	}
	ctx.Data["NumFollowing"] = numFollowing

	archived := ctx.FormOptionalBool("archived")
	ctx.Data["IsArchived"] = archived

	fork := ctx.FormOptionalBool("fork")
	ctx.Data["IsFork"] = fork

	mirror := ctx.FormOptionalBool("mirror")
	ctx.Data["IsMirror"] = mirror

	template := ctx.FormOptionalBool("template")
	ctx.Data["IsTemplate"] = template

	private := ctx.FormOptionalBool("private")
	ctx.Data["IsPrivate"] = private

	switch tab {
	case "followers":
		ctx.Data["Cards"] = followers
		total = numFollowers
	case "following":
		ctx.Data["Cards"] = following
		total = numFollowing
	case "activity":
		if setting.Service.EnableUserHeatmap && activities_model.ActivityReadable(ctx.ContextUser, ctx.Doer) {
			ctx.Data["EnableHeatmap"] = true
			ctx.Data["HeatmapURL"] = ctx.ContextUser.HomeLink() + "/-/heatmap"
		}

		date := ctx.FormString("date")
		pagingNum = setting.UI.FeedPagingNum
		showPrivate := ctx.IsSigned && (ctx.Doer.IsAdmin || ctx.Doer.ID == ctx.ContextUser.ID)
		// a public-only API token must not surface private activity, even for its own owner
		if showPrivate && context.TokenIsPublicOnly(ctx) {
			showPrivate = false
		}
		items, feedCount, err := feed_service.GetFeedsForDashboard(ctx, activities_model.GetFeedsOptions{
			RequestedUser:   ctx.ContextUser,
			Actor:           ctx.Doer,
			IncludePrivate:  showPrivate,
			OnlyPerformedBy: true,
			IncludeDeleted:  false,
			Date:            date,
			ListOptions: db.ListOptions{
				PageSize: pagingNum,
				Page:     page,
			},
		})
		if err != nil {
			ctx.ServerError("GetFeeds", err)
			return
		}
		ctx.Data["Feeds"] = items
		ctx.Data["Date"] = date
		curRows = len(items)
		total = feedCount
	case "stars":
		ctx.Data["PageIsProfileStarList"] = true
		ctx.Data["ShowRepoOwnerOnList"] = true
		repos, count, err = repo_model.SearchRepository(ctx, repo_model.SearchRepoOptions{
			ListOptions: db.ListOptions{
				PageSize: pagingNum,
				Page:     page,
			},
			Actor:              ctx.Doer,
			Keyword:            keyword,
			OrderBy:            orderBy,
			Private:            ctx.IsSigned,
			StarredByID:        ctx.ContextUser.ID,
			Collaborate:        optional.Some(false),
			TopicOnly:          topicOnly,
			Language:           language,
			IncludeDescription: setting.UI.SearchRepoDescription,
			Archived:           archived,
			Fork:               fork,
			Mirror:             mirror,
			Template:           template,
			IsPrivate:          private,
		})
		if err != nil {
			ctx.ServerError("SearchRepository", err)
			return
		}

		total = count
	case "watching":
		repos, count, err = repo_model.SearchRepository(ctx, repo_model.SearchRepoOptions{
			ListOptions: db.ListOptions{
				PageSize: pagingNum,
				Page:     page,
			},
			Actor:              ctx.Doer,
			Keyword:            keyword,
			OrderBy:            orderBy,
			Private:            ctx.IsSigned,
			WatchedByID:        ctx.ContextUser.ID,
			Collaborate:        optional.Some(false),
			TopicOnly:          topicOnly,
			Language:           language,
			IncludeDescription: setting.UI.SearchRepoDescription,
			Archived:           archived,
			Fork:               fork,
			Mirror:             mirror,
			Template:           template,
			IsPrivate:          private,
		})
		if err != nil {
			ctx.ServerError("SearchRepository", err)
			return
		}

		total = count
	case "overview":
		if bytes, err := profileReadme.GetBlobContent(setting.UI.MaxDisplayFileSize); err != nil {
			log.Error("failed to GetBlobContent: %v", err)
		} else {
			rctx := renderhelper.NewRenderContextRepoFile(ctx, profileDbRepo, renderhelper.RepoFileOptions{
				CurrentRefSubURL: git.RefNameFromBranch(profileDbRepo.DefaultBranch).RefWebLinkPath(),
			})
			if profileContent, err := markdown.RenderString(rctx, bytes); err != nil {
				log.Error("failed to RenderString: %v", err)
			} else {
				ctx.Data["ProfileReadmeContent"] = profileContent
			}
		}
	case "organizations":
		orgs, count, err := db.FindAndCount[organization.Organization](ctx, organization.FindOrgOptions{
			UserID:            ctx.ContextUser.ID,
			IncludeVisibility: organization.DoerViewOtherVisibility(ctx.Doer, ctx.ContextUser),
			ListOptions: db.ListOptions{
				Page:     page,
				PageSize: pagingNum,
			},
		})
		if err != nil {
			ctx.ServerError("GetUserOrganizations", err)
			return
		}
		ctx.Data["Cards"] = orgs
		total = count
	case "gists":
		// cgit addition: gists are mirrored in by migrate_gists.sh as repos named
		// "gist-<id>" (see that script's comment for why the id, not the
		// description, is what's stable) — this tab is just the repositories
		// tab's query with the name filter flipped, same as GitHub separating
		// Repositories from Gists on a profile.
		ctx.Data["PageIsProfileGistList"] = true
		repos, count, err = repo_model.SearchRepository(ctx, repo_model.SearchRepoOptions{
			ListOptions: db.ListOptions{
				PageSize: pagingNum,
				Page:     page,
			},
			Actor:              ctx.Doer,
			Keyword:            keyword,
			OwnerID:            ctx.ContextUser.ID,
			OrderBy:            orderBy,
			Private:            ctx.IsSigned,
			Collaborate:        optional.Some(false),
			TopicOnly:          topicOnly,
			Language:           language,
			IncludeDescription: setting.UI.SearchRepoDescription,
			Archived:           archived,
			Fork:               fork,
			Mirror:             mirror,
			Template:           template,
			IsPrivate:          private,
			NamePrefix:         "gist-",
		})
		if err != nil {
			ctx.ServerError("SearchRepository", err)
			return
		}

		total = count
	default: // default to "repositories"
		repos, count, err = repo_model.SearchRepository(ctx, repo_model.SearchRepoOptions{
			ListOptions: db.ListOptions{
				PageSize: pagingNum,
				Page:     page,
			},
			Actor:              ctx.Doer,
			Keyword:            keyword,
			OwnerID:            ctx.ContextUser.ID,
			OrderBy:            orderBy,
			Private:            ctx.IsSigned,
			Collaborate:        optional.Some(false),
			TopicOnly:          topicOnly,
			Language:           language,
			IncludeDescription: setting.UI.SearchRepoDescription,
			Archived:           archived,
			Fork:               fork,
			Mirror:             mirror,
			Template:           template,
			IsPrivate:          private,
			// cgit addition: gists get their own tab above, so they're excluded here.
			ExcludeNamePrefix: "gist-",
		})
		if err != nil {
			ctx.ServerError("SearchRepository", err)
			return
		}

		total = count
	}
	ctx.Data["Repos"] = repos
	ctx.Data["Total"] = total

	// cgit addition: file-content previews for the Gists tab cards, built
	// only when that tab is actually being rendered — every other tab
	// leaves GistPreviews unset and its template branch never looks it up.
	if tab == "gists" {
		ctx.Data["GistPreviews"] = buildGistPreviews(ctx, repos)
	}

	if _, err := shared_user.RenderUserOrgHeader(ctx); err != nil {
		ctx.ServerError("RenderUserOrgHeader", err)
		return
	}

	pager := context.NewPagination(total, pagingNum, page, 5)
	if tab == "activity" {
		// FIXME: UNLIMITE-PAGING-ONE-MORE-ROW: see another comment
		pager.WithUnlimitedPaging(curRows, curRows == pagingNum)
	}
	pager.AddParamFromRequest(ctx.Req)
	ctx.Data["Page"] = pager
}

// ActionUserFollow is for follow/unfollow user request
func ActionUserFollow(ctx *context.Context) {
	var err error
	switch ctx.FormString("action") {
	case "follow":
		err = user_model.FollowUser(ctx, ctx.Doer, ctx.ContextUser)
	case "unfollow":
		err = user_model.UnfollowUser(ctx, ctx.Doer.ID, ctx.ContextUser.ID)
	}

	if err != nil {
		log.Error("Failed to apply action %q: %v", ctx.FormString("action"), err)
		ctx.HTTPError(http.StatusBadRequest, fmt.Sprintf("Action %q failed", ctx.FormString("action")))
		return
	}
	if _, err := shared_user.RenderUserOrgHeader(ctx); err != nil {
		ctx.ServerError("RenderUserOrgHeader", err)
		return
	}
	if ctx.ContextUser.IsIndividual() {
		ctx.HTML(http.StatusOK, tplProfileBigAvatar)
		return
	} else if ctx.ContextUser.IsOrganization() {
		ctx.Data["Org"] = ctx.ContextUser
		ctx.Data["IsFollowing"] = ctx.Doer != nil && user_model.IsFollowing(ctx, ctx.Doer.ID, ctx.ContextUser.ID)
		ctx.HTML(http.StatusOK, tplFollowUnfollow)
		return
	}
	log.Error("Failed to apply action %q: unsupported context user type: %s", ctx.FormString("action"), ctx.ContextUser.Type)
	ctx.HTTPError(http.StatusBadRequest, fmt.Sprintf("Action %q failed", ctx.FormString("action")))
}

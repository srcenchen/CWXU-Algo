// Package blogaccess holds pure blog article access and org-sync rules.
// Keep free of HTTP / DB so unit tests can drive the real policy path.
package blogaccess

import (
	"strings"

	"cwxu-algo/app/common/blogtext"
)

// Visibility values stored on articles.
const (
	VisibilityPublic   = "public"
	VisibilityPrivate  = "private"
	VisibilityPassword = "password"
)

// ValidVisibility reports whether v is a known visibility string.
func ValidVisibility(v string) bool {
	switch NormalizeVisibility(v) {
	case VisibilityPublic, VisibilityPrivate, VisibilityPassword:
		return true
	default:
		return false
	}
}

// NormalizeVisibility trims and lowercases; empty becomes public.
func NormalizeVisibility(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return VisibilityPublic
	}
	// aliases
	if s == "unlisted" || s == "hidden" {
		return VisibilityPrivate
	}
	return s
}

// ArticleAccess is the minimal input for access decisions.
type ArticleAccess struct {
	Visibility   string
	OwnerID      uint
	// HasPassword is true when a password hash is set (password visibility).
	HasPassword bool
}

// Decision is the result of evaluating viewer rights on an article.
type Decision struct {
	// CanSeeMeta: title / cover / dates / counts (list cards).
	CanSeeMeta bool
	// CanSeeBody: full markdown body.
	CanSeeBody bool
	// RequiresPassword: body locked until correct password / unlock token.
	RequiresPassword bool
	// Reason is a stable machine code for clients (empty when fully open).
	Reason string
}

// Evaluate returns what a viewer may see.
// passwordOK is true only after the server verified the article password
// (or a valid unlock token). Owner always sees body.
func Evaluate(a ArticleAccess, viewerID uint, passwordOK bool) Decision {
	vis := NormalizeVisibility(a.Visibility)
	isOwner := a.OwnerID != 0 && viewerID == a.OwnerID

	if isOwner {
		return Decision{CanSeeMeta: true, CanSeeBody: true}
	}

	switch vis {
	case VisibilityPublic:
		return Decision{CanSeeMeta: true, CanSeeBody: true}
	case VisibilityPrivate:
		return Decision{
			CanSeeMeta: false,
			CanSeeBody: false,
			Reason:     "private",
		}
	case VisibilityPassword:
		if passwordOK {
			return Decision{CanSeeMeta: true, CanSeeBody: true}
		}
		// Non-owners may see teaser meta (title/cover) but not body.
		return Decision{
			CanSeeMeta:       true,
			CanSeeBody:       false,
			RequiresPassword: true,
			Reason:           "password_required",
		}
	default:
		return Decision{
			CanSeeMeta: false,
			CanSeeBody: false,
			Reason:     "unknown_visibility",
		}
	}
}

// CanManage is true for the author (or site admin when allowAdmin).
func CanManage(ownerID, viewerID uint, isSiteAdmin bool) bool {
	if viewerID == 0 {
		return false
	}
	if ownerID != 0 && viewerID == ownerID {
		return true
	}
	return isSiteAdmin
}

// ExpandSyncOrgIDs applies the product rule:
// syncing to any private (non-system) org automatically also exposes to the public domain.
// selected may contain publicOrgID already; result is de-duplicated and stable order:
// public first if present, then remaining IDs sorted by first appearance.
func ExpandSyncOrgIDs(selected []uint, publicOrgID uint, isSystemOrg func(orgID uint) bool) []uint {
	if len(selected) == 0 {
		return nil
	}
	seen := make(map[uint]struct{}, len(selected)+1)
	out := make([]uint, 0, len(selected)+1)
	hasPrivate := false
	for _, id := range selected {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if publicOrgID != 0 && id == publicOrgID {
			continue
		}
		if isSystemOrg == nil || !isSystemOrg(id) {
			hasPrivate = true
		}
	}
	if hasPrivate && publicOrgID != 0 {
		if _, ok := seen[publicOrgID]; !ok {
			// prepend public domain so it is always explicit
			out = append([]uint{publicOrgID}, out...)
		}
	}
	return out
}

// ThemeEnabled resolves whether custom theme is active for a user.
// globalAll: site-wide open flag; perUser: optional per-user override (nil = inherit).
func ThemeEnabled(globalAll bool, perUser *bool) bool {
	if perUser != nil {
		return *perUser
	}
	return globalAll
}

// AutoSurface reports whether a public non-password article should auto-appear
// on discovery recommend, plaza, and author-org surfaces.
func AutoSurface(visibility string) bool {
	return blogtext.AutoSurface(visibility)
}

// DefaultSummaryMaxRunes mirrors blogtext.
const DefaultSummaryMaxRunes = blogtext.DefaultSummaryMaxRunes

// DefaultSummary builds a content-derived brief for list cards.
func DefaultSummary(content string) string { return blogtext.DefaultSummary(content) }

// IsDefaultSummary reports whether summary is system-generated (always true).
func IsDefaultSummary(summary, content string) bool {
	return blogtext.IsDefaultSummary(summary, content)
}

// ResolveSummaryForSave always regenerates from content; userSummary ignored.
func ResolveSummaryForSave(userSummary, content string) string {
	return blogtext.ResolveSummaryForSave(userSummary, content)
}

package service

import "github.com/google/wire"

// ProviderSet is service providers.
var ProviderSet = wire.NewSet(
	NewAuthService,
	NewProfileService,
	NewGroupService,
	NewRoleService,
	NewSiteService,
	NewOrgService,
	NewRbacService,
	NewPasteService,
	NewSocialService,
	NewNotificationService,
	NewBlogService,
	NewSEOService,
	NewSubscriptionService,
	NewTicketService,
	NewLuoguPluginService,
)

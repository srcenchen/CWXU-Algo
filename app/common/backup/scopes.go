// Package backup implements GoAlgo full-site logical backup (export/import).
package backup

import (
	"fmt"
	"sort"
	"strings"
)

const (
	FormatVersion = "goalgo-backup-v1"
	ConfirmToken  = "RESTORE"

	ScopeAll        = "all"
	ScopeSite       = "site"
	ScopeUsers      = "users"
	ScopeOrgs       = "orgs"
	ScopePastes     = "pastes"
	ScopeVisits     = "visits"
	ScopePlatforms  = "platforms"
	ScopeSubmits    = "submits"
	ScopeContests   = "contests"
	ScopeProblems   = "problems"
	ScopeBulletins  = "bulletins"
	ScopeEmergency  = "emergency"
	ScopeDailyStats = "daily_stats"
	ScopeUserAC     = "user_ac"
	ScopeFiles      = "files"
)

// TableSpec describes one logical table in the backup package.
type TableSpec struct {
	File  string // relative path under data/, e.g. users.ndjson
	DB    string // "user" | "core"
	Table string // physical table name
	Scope string
	// SeqCol is the identity column for setval after import; empty = no sequence fix.
	SeqCol string
}

// AllTableSpecs export/import order (dependencies first for import).
var AllTableSpecs = []TableSpec{
	{File: "site_configs.ndjson", DB: "user", Table: "site_configs", Scope: ScopeSite, SeqCol: "id"},
	// GORM 默认将 PlanQuota 复数化为 plan_quota（quota 不规则），勿写成 plan_quotas
	{File: "plan_quota.ndjson", DB: "user", Table: "plan_quota", Scope: ScopeOrgs, SeqCol: "id"},
	{File: "users.ndjson", DB: "user", Table: "users", Scope: ScopeUsers, SeqCol: "id"},
	{File: "orgs.ndjson", DB: "user", Table: "orgs", Scope: ScopeOrgs, SeqCol: "id"},
	{File: "groups.ndjson", DB: "user", Table: "groups", Scope: ScopeOrgs, SeqCol: "id"},
	{File: "org_members.ndjson", DB: "user", Table: "org_members", Scope: ScopeOrgs, SeqCol: "id"},
	{File: "org_join_requests.ndjson", DB: "user", Table: "org_join_requests", Scope: ScopeOrgs, SeqCol: "id"},
	// RBAC：roles 先于 role_permissions / user_roles（外键依赖顺序）
	{File: "roles.ndjson", DB: "user", Table: "roles", Scope: ScopeOrgs, SeqCol: "id"},
	{File: "role_permissions.ndjson", DB: "user", Table: "role_permissions", Scope: ScopeOrgs, SeqCol: "id"},
	{File: "user_roles.ndjson", DB: "user", Table: "user_roles", Scope: ScopeOrgs, SeqCol: "id"},
	{File: "org_role_perms.ndjson", DB: "user", Table: "org_role_perms", Scope: ScopeOrgs, SeqCol: "id"},
	{File: "pastes.ndjson", DB: "user", Table: "pastes", Scope: ScopePastes, SeqCol: "id"},
	{File: "site_visit_dailies.ndjson", DB: "user", Table: "site_visit_dailies", Scope: ScopeVisits, SeqCol: "id"},
	{File: "platforms.ndjson", DB: "core", Table: "platforms", Scope: ScopePlatforms, SeqCol: "id"},
	{File: "problems.ndjson", DB: "core", Table: "problems", Scope: ScopeProblems, SeqCol: "id"},
	{File: "submit_logs.ndjson", DB: "core", Table: "submit_logs", Scope: ScopeSubmits, SeqCol: "id"},
	{File: "contest_logs.ndjson", DB: "core", Table: "contest_logs", Scope: ScopeContests, SeqCol: "id"},
	{File: "daily_user_stats.ndjson", DB: "core", Table: "daily_user_stats", Scope: ScopeDailyStats, SeqCol: ""},
	{File: "user_ac_problems.ndjson", DB: "core", Table: "user_ac_problems", Scope: ScopeUserAC, SeqCol: ""},
	{File: "user_ac_problem_days.ndjson", DB: "core", Table: "user_ac_problem_days", Scope: ScopeUserAC, SeqCol: ""},
	{File: "bulletins.ndjson", DB: "core", Table: "bulletins", Scope: ScopeBulletins, SeqCol: "id"},
	{File: "emergency_notices.ndjson", DB: "core", Table: "emergency_notices", Scope: ScopeEmergency, SeqCol: "id"},
}

// ValidScopes returns known non-all scopes.
func ValidScopes() []string {
	return []string{
		ScopeSite, ScopeUsers, ScopeOrgs, ScopePastes, ScopeVisits,
		ScopePlatforms, ScopeSubmits, ScopeContests, ScopeProblems,
		ScopeBulletins, ScopeEmergency, ScopeDailyStats, ScopeUserAC, ScopeFiles,
	}
}

// NormalizeScopes expands "all", dedupes, validates. Empty → all.
func NormalizeScopes(in []string) ([]string, error) {
	if len(in) == 0 {
		return []string{ScopeAll}, nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(strings.ToLower(s))
		if s == "" {
			continue
		}
		if s == ScopeAll {
			return []string{ScopeAll}, nil
		}
		ok := false
		for _, v := range ValidScopes() {
			if s == v {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("未知 scope: %s", s)
		}
		if _, exists := seen[s]; exists {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{ScopeAll}, nil
	}
	sort.Strings(out)
	return out, nil
}

// ExpandedScopes returns concrete scopes (never contains "all").
func ExpandedScopes(scopes []string) []string {
	norm, err := NormalizeScopes(scopes)
	if err != nil || len(norm) == 0 || (len(norm) == 1 && norm[0] == ScopeAll) {
		return append([]string{}, ValidScopes()...)
	}
	return norm
}

// HasScope reports whether concrete list includes s.
func HasScope(concrete []string, s string) bool {
	for _, x := range concrete {
		if x == s {
			return true
		}
	}
	return false
}

// NeedsCoreDB reports whether any selected scope touches algo_core_data.
func NeedsCoreDB(concrete []string) bool {
	for _, s := range concrete {
		switch s {
		case ScopePlatforms, ScopeSubmits, ScopeContests, ScopeProblems,
			ScopeBulletins, ScopeEmergency, ScopeDailyStats, ScopeUserAC:
			return true
		}
	}
	return false
}

// TablesForScopes returns table specs for selected scopes (import order).
func TablesForScopes(concrete []string) []TableSpec {
	var out []TableSpec
	for _, t := range AllTableSpecs {
		if HasScope(concrete, t.Scope) {
			out = append(out, t)
		}
	}
	return out
}

// Manifest describes a backup package.
type Manifest struct {
	Version      string           `json:"version"`
	CreatedAt    string           `json:"createdAt"`
	Scopes       []string         `json:"scopes"`
	IncludeFiles bool             `json:"includeFiles"`
	TableCounts  map[string]int64 `json:"tableCounts"`
	FileCount    int              `json:"fileCount,omitempty"`
	AppHint      string           `json:"appHint,omitempty"`
}

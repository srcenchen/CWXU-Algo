package model

import "testing"

func TestOrgRoleRankOrder(t *testing.T) {
	if OrgRoleRank(OrgRoleMember) >= OrgRoleRank(OrgRoleCaptain) {
		t.Fatal("member < captain")
	}
	if OrgRoleRank(OrgRoleCaptain) >= OrgRoleRank(OrgRoleGroupLeader) {
		t.Fatal("captain < group_leader")
	}
	if OrgRoleRank(OrgRoleGroupLeader) >= OrgRoleRank(OrgRoleCoach) {
		t.Fatal("group_leader < coach")
	}
	if OrgRoleRank(OrgRoleCoach) >= OrgRoleRank(OrgRoleOrgAdmin) {
		t.Fatal("coach < org_admin")
	}
}

func TestCanAppointOrgRole(t *testing.T) {
	// 教练可任命组长/队长/成员
	if !CanAppointOrgRole(OrgRoleCoach, OrgRoleMember, OrgRoleCaptain) {
		t.Fatal("coach should appoint captain")
	}
	if !CanAppointOrgRole(OrgRoleCoach, OrgRoleMember, OrgRoleGroupLeader) {
		t.Fatal("coach should appoint group_leader")
	}
	// 教练不可任命组织管理员或教练
	if CanAppointOrgRole(OrgRoleCoach, OrgRoleMember, OrgRoleOrgAdmin) {
		t.Fatal("coach must not appoint org_admin")
	}
	if CanAppointOrgRole(OrgRoleCoach, OrgRoleMember, OrgRoleCoach) {
		t.Fatal("coach must not appoint coach")
	}
	// 不可改同级/更高
	if CanAppointOrgRole(OrgRoleCoach, OrgRoleCoach, OrgRoleMember) {
		t.Fatal("coach must not demote peer coach")
	}
	if CanAppointOrgRole(OrgRoleCoach, OrgRoleOrgAdmin, OrgRoleMember) {
		t.Fatal("coach must not demote org_admin")
	}
	// 组长可任命队长
	if !CanAppointOrgRole(OrgRoleGroupLeader, OrgRoleMember, OrgRoleCaptain) {
		t.Fatal("group_leader should appoint captain")
	}
	// 组长不可任命组长
	if CanAppointOrgRole(OrgRoleGroupLeader, OrgRoleMember, OrgRoleGroupLeader) {
		t.Fatal("group_leader must not appoint group_leader")
	}
	// 队长无任命权
	if CanAppointOrgRole(OrgRoleCaptain, OrgRoleMember, OrgRoleMember) {
		t.Fatal("captain must not appoint")
	}
}

func TestRoleNeedsScope(t *testing.T) {
	if st, ok := RoleNeedsScope(OrgRoleCaptain); !ok || st != ScopeTypeSquad {
		t.Fatalf("captain needs squad, got %s %v", st, ok)
	}
	if st, ok := RoleNeedsScope(OrgRoleGroupLeader); !ok || st != ScopeTypeGroup {
		t.Fatalf("group_leader needs group, got %s %v", st, ok)
	}
	if _, ok := RoleNeedsScope(OrgRoleCoach); ok {
		t.Fatal("coach needs no scope")
	}
}

func TestIsOrgFullScopeRole(t *testing.T) {
	if !IsOrgFullScopeRole(OrgRoleCoach) || !IsOrgFullScopeRole(OrgRoleOrgAdmin) {
		t.Fatal("coach/org_admin full scope")
	}
	if IsOrgFullScopeRole(OrgRoleGroupLeader) || IsOrgFullScopeRole(OrgRoleCaptain) {
		t.Fatal("group_leader/captain not full scope")
	}
}

func TestEffectiveRoleFromGrants(t *testing.T) {
	if EffectiveRoleFromGrants(OrgRoleCoach, true, true) != OrgRoleCoach {
		t.Fatal("coach stays coach")
	}
	if EffectiveRoleFromGrants(OrgRoleMember, true, true) != OrgRoleGroupLeader {
		t.Fatal("group grant wins over squad")
	}
	if EffectiveRoleFromGrants(OrgRoleMember, false, true) != OrgRoleCaptain {
		t.Fatal("squad only → captain")
	}
	if EffectiveRoleFromGrants(OrgRoleCaptain, false, false) != OrgRoleMember {
		t.Fatal("no grant demotes captain")
	}
	if EffectiveRoleFromGrants(OrgRoleGroupLeader, false, true) != OrgRoleCaptain {
		t.Fatal("lost group keep squad → captain")
	}
}

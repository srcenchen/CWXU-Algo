package rbac

import "testing"

// 注册表完整性：bit 唯一且非负、code 唯一、分组存在、scope 合法
func TestRegistryIntegrity(t *testing.T) {
	bits := map[int]string{}
	codes := map[string]bool{}
	groups := map[string]bool{}
	for _, g := range groupMeta {
		groups[g.Key] = true
	}
	for _, p := range All() {
		if p.Bit < 0 {
			t.Fatalf("perm %s 非法 bit %d", p.Code, p.Bit)
		}
		if prev, dup := bits[p.Bit]; dup {
			t.Fatalf("bit %d 重复：%s 与 %s", p.Bit, prev, p.Code)
		}
		bits[p.Bit] = p.Code
		if codes[p.Code] {
			t.Fatalf("code 重复：%s", p.Code)
		}
		codes[p.Code] = true
		if !groups[p.Group] {
			t.Fatalf("perm %s 引用未定义分组 %s", p.Code, p.Group)
		}
		if p.Scope != ScopeSite && p.Scope != ScopeOrg {
			t.Fatalf("perm %s 非法 scope %s", p.Code, p.Scope)
		}
	}
}

// 系统角色模板引用的权限必须全部已注册，且作用域匹配
func TestSystemRolesValid(t *testing.T) {
	for _, r := range SystemRoles() {
		for _, code := range r.Perms {
			p, ok := ByCode(code)
			if !ok {
				t.Fatalf("角色 %s 引用未注册权限 %s", r.Code, code)
			}
			// 站点管理员含全部权限；其余角色权限作用域须与角色一致
			if r.Code != RoleSiteAdmin && p.Scope != r.Scope {
				t.Fatalf("角色 %s(scope=%s) 引用异域权限 %s(scope=%s)", r.Code, r.Scope, code, p.Scope)
			}
		}
	}
	if !IsSystemRoleCode("org_admin") || IsSystemRoleCode("c_custom") {
		t.Fatal("IsSystemRoleCode 判定异常")
	}
}

func TestMaskRoundTrip(t *testing.T) {
	codes := []string{PermOrgBulletinManage, PermSiteConfigWrite, PermOrgMemberEmail}
	mask := Encode(codes)
	set, ok := Decode(mask)
	if !ok {
		t.Fatal("decode 失败")
	}
	if len(set) != len(codes) {
		t.Fatalf("roundtrip 数量不符：want %d got %d", len(codes), len(set))
	}
	for _, c := range codes {
		if !set[c] {
			t.Fatalf("roundtrip 丢失 %s", c)
		}
		has, valid := MaskHas(mask, c)
		if !valid || !has {
			t.Fatalf("MaskHas(%s) = %v,%v", c, has, valid)
		}
	}
	if has, valid := MaskHas(mask, PermSiteOrgDelete); !valid || has {
		t.Fatalf("未授予权限不应命中: %v,%v", has, valid)
	}
}

func TestMaskEdgeCases(t *testing.T) {
	// 空集合可编码且全 false
	empty := Encode(nil)
	if empty == "" {
		t.Fatal("空集合应产生非空位图")
	}
	if has, valid := MaskHas(empty, PermSiteConfigRead); !valid || has {
		t.Fatalf("空位图: %v,%v", has, valid)
	}
	// 非法 base64 → invalid，走旧字段推导
	if _, valid := MaskHas("!!!not-base64!!!", PermSiteConfigRead); valid {
		t.Fatal("非法位图应 valid=false")
	}
	if _, valid := MaskHas("", PermSiteConfigRead); valid {
		t.Fatal("空位图串应 valid=false")
	}
	// 未注册权限：位图有效但恒 false
	if has, valid := MaskHas(Encode(AllCodes()), "no.such.perm"); !valid || has {
		t.Fatalf("未注册权限: %v,%v", has, valid)
	}
	// 短位图（旧 token 未含新增位）：新权限默认关闭
	short := "AA" // 1 字节全 0
	if has, valid := MaskHas(short, PermOrgMemberEmail); !valid || has {
		t.Fatalf("短位图高位应为 false: %v,%v", has, valid)
	}
}

func TestLegacyHas(t *testing.T) {
	if !LegacyHas(PermOrgBulletinManage, "coach") {
		t.Fatal("教练应有组织公告")
	}
	if LegacyHas(PermOrgMemberRole, "coach") {
		t.Fatal("教练不应有成员任命")
	}
	if !LegacyHas(PermOrgMemberRole, "org_admin") {
		t.Fatal("团队管理员应有成员任命")
	}
	if LegacyHas(PermOrgGroupManage, "member") {
		t.Fatal("成员不应有分组管理")
	}
	if LegacyHas(PermOrgGroupManage, "") {
		t.Fatal("无组织角色不应有权限")
	}
}

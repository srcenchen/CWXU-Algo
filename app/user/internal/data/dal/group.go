package dal

import (
	"context"
	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

type GroupDal struct {
	db *gorm.DB
}

func NewGroupDal(data *data.Data) *GroupDal {
	return &GroupDal{db: data.DB}
}

func (d *GroupDal) Create(ctx context.Context, name, describe string, orgID uint) (int64, error) {
	group := model.Group{
		Name:     &name,
		Describe: describe,
		OrgID:    orgID,
	}
	if err := d.db.WithContext(ctx).Create(&group).Error; err != nil {
		return 0, fmt.Errorf("创建组失败: %w", err)
	}
	return int64(group.ID), nil
}

// EnsureDefaultGroup 确保组织有「默认分组」，返回其 ID
func (d *GroupDal) EnsureDefaultGroup(ctx context.Context, orgID uint) (uint, error) {
	if orgID == 0 {
		return 0, fmt.Errorf("orgID 无效")
	}
	var g model.Group
	err := d.db.WithContext(ctx).
		Where("org_id = ? AND name IN ?", orgID, []string{model.DefaultGroupName, "未分组"}).
		Order("id ASC").
		First(&g).Error
	if err == nil {
		if g.Name != nil && *g.Name == "未分组" {
			name := model.DefaultGroupName
			_ = d.db.WithContext(ctx).Model(&g).Updates(map[string]interface{}{
				"name":     name,
				"describe": model.DefaultGroupDesc,
			}).Error
		}
		return g.ID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	name := model.DefaultGroupName
	g = model.Group{
		Name:     &name,
		Describe: model.DefaultGroupDesc,
		OrgID:    orgID,
	}
	if err := d.db.WithContext(ctx).Create(&g).Error; err != nil {
		return 0, err
	}
	return g.ID, nil
}

func (d *GroupDal) Delete(ctx context.Context, id int64) error {
	var g model.Group
	if err := d.db.WithContext(ctx).First(&g, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("组不存在")
		}
		return fmt.Errorf("查询组失败: %w", err)
	}
	if g.IsDefaultGroup() {
		return fmt.Errorf("不能删除默认分组")
	}
	defaultID, err := d.EnsureDefaultGroup(ctx, g.OrgID)
	if err != nil {
		return fmt.Errorf("准备默认分组失败: %w", err)
	}

	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. 成员迁回默认分组
		if err := tx.Model(&model.OrgMember{}).
			Where("group_id = ?", id).
			Update("group_id", defaultID).Error; err != nil {
			return fmt.Errorf("迁移成员到默认分组失败: %w", err)
		}

		// 2. 解散组内分队：先卸任队长 → 清成员/scope → 删分队
		var squadIDs []uint
		_ = tx.Model(&model.Squad{}).Where("group_id = ? AND org_id = ?", id, g.OrgID).
			Pluck("id", &squadIDs).Error
		if len(squadIDs) > 0 {
			var captainUIDs []uint
			_ = tx.Model(&model.OrgScopeGrant{}).
				Where("org_id = ? AND scope_type = ? AND scope_id IN ?",
					g.OrgID, model.ScopeTypeSquad, squadIDs).
				Pluck("user_id", &captainUIDs).Error
			if len(captainUIDs) > 0 {
				_ = tx.Model(&model.OrgMember{}).
					Where("org_id = ? AND user_id IN ? AND role = ?",
						g.OrgID, captainUIDs, model.OrgRoleCaptain).
					Update("role", model.OrgRoleMember).Error
			}
			if err := tx.Where("squad_id IN ?", squadIDs).Delete(&model.SquadMember{}).Error; err != nil {
				return fmt.Errorf("清理分队成员失败: %w", err)
			}
			if err := tx.Where("org_id = ? AND scope_type = ? AND scope_id IN ?",
				g.OrgID, model.ScopeTypeSquad, squadIDs).
				Delete(&model.OrgScopeGrant{}).Error; err != nil {
				return fmt.Errorf("清理分队管理范围失败: %w", err)
			}
			if err := tx.Where("id IN ?", squadIDs).Delete(&model.Squad{}).Error; err != nil {
				return fmt.Errorf("删除分队失败: %w", err)
			}
		}

		// 3. 卸任本组组长 → 清指向本分组的 scope
		var leaderUIDs []uint
		_ = tx.Model(&model.OrgScopeGrant{}).
			Where("org_id = ? AND scope_type = ? AND scope_id = ?", g.OrgID, model.ScopeTypeGroup, id).
			Pluck("user_id", &leaderUIDs).Error
		if len(leaderUIDs) > 0 {
			_ = tx.Model(&model.OrgMember{}).
				Where("org_id = ? AND user_id IN ? AND role = ?", g.OrgID, leaderUIDs, model.OrgRoleGroupLeader).
				Update("role", model.OrgRoleMember).Error
		}
		if err := tx.Where("org_id = ? AND scope_type = ? AND scope_id = ?",
			g.OrgID, model.ScopeTypeGroup, id).
			Delete(&model.OrgScopeGrant{}).Error; err != nil {
			return fmt.Errorf("清理分组管理范围失败: %w", err)
		}

		// 4. 删分组
		result := tx.Delete(&model.Group{}, id)
		if result.Error != nil {
			return fmt.Errorf("删除组失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("组不存在")
		}
		return nil
	})
}

func (d *GroupDal) Get(ctx context.Context, id int64) (*model.Group, error) {
	var group model.Group
	err := d.db.WithContext(ctx).First(&group, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("组不存在")
	}
	if err != nil {
		return nil, fmt.Errorf("查询组失败: %w", err)
	}
	return &group, nil
}

// OrgDisplayNames 批量取组织内名称
func (d *GroupDal) OrgDisplayNames(ctx context.Context, orgID uint, userIDs []uint) map[uint]string {
	out := make(map[uint]string)
	if orgID == 0 || len(userIDs) == 0 {
		return out
	}
	type row struct {
		UserID         uint
		OrgDisplayName string
	}
	var rows []row
	_ = d.db.WithContext(ctx).Model(&model.OrgMember{}).
		Select("user_id, org_display_name").
		Where("org_id = ? AND user_id IN ?", orgID, userIDs).
		Find(&rows).Error
	for _, r := range rows {
		if n := strings.TrimSpace(r.OrgDisplayName); n != "" {
			out[r.UserID] = n
		}
	}
	return out
}

// GetWithUsers 分组详情 + 成员分页（page 从 1；pageSize 默认由调用方约束）
func (d *GroupDal) GetWithUsers(ctx context.Context, id, page, pageSize int64) (*model.Group, []model.User, int64, error) {
	var group model.Group
	err := d.db.WithContext(ctx).First(&group, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, 0, fmt.Errorf("组不存在")
	}
	if err != nil {
		return nil, nil, 0, fmt.Errorf("查询组失败: %w", err)
	}
	var total int64
	if err := d.db.WithContext(ctx).Model(&model.OrgMember{}).
		Where("org_id = ? AND group_id = ?", group.OrgID, id).
		Count(&total).Error; err != nil {
		return nil, nil, 0, fmt.Errorf("查询组成员总数失败: %w", err)
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize
	var users []model.User
	if err := d.db.WithContext(ctx).Table("users AS u").
		Select("u.id, u.username, u.name, u.avatar, COALESCE(m.group_id, 0) AS group_id").
		Joins("JOIN org_members AS m ON m.user_id = u.id").
		Where("m.org_id = ? AND m.group_id = ?", group.OrgID, id).
		Order("u.id").
		Limit(int(pageSize)).
		Offset(int(offset)).
		Find(&users).Error; err != nil {
		return nil, nil, 0, fmt.Errorf("查询组成员失败: %w", err)
	}
	return &group, users, total, nil
}

func (d *GroupDal) List(ctx context.Context, page, size int64, orgID uint) ([]model.Group, int64, error) {
	var list []model.Group
	var total int64

	// id=0 为历史虚拟「未分组」，不计入列表
	q := d.db.WithContext(ctx).Model(&model.Group{}).Where("id > 0")
	if orgID > 0 {
		q = q.Where("org_id = ?", orgID)
	}
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("查询组总数失败: %w", err)
	}

	offset := (page - 1) * size
	lq := d.db.WithContext(ctx).Where("id > 0").Order("id DESC").Limit(int(size)).Offset(int(offset))
	if orgID > 0 {
		lq = lq.Where("org_id = ?", orgID)
	}
	if err := lq.Find(&list).Error; err != nil {
		return nil, 0, fmt.Errorf("查询组列表失败: %w", err)
	}

	return list, total, nil
}

func (d *GroupDal) Update(ctx context.Context, id int64, name, describe string) error {
	updates := map[string]interface{}{}
	if name != "" {
		updates["name"] = name
	}
	if describe != "" {
		updates["describe"] = describe
	}

	result := d.db.WithContext(ctx).Model(&model.Group{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("更新组失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("组不存在")
	}
	return nil
}

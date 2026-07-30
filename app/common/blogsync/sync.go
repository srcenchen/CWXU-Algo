// Package blogsync 让 core 将主站题解同步到用户博客（algo_user.blog_*）。
// 与 notify 相同：core 可选连 user 库，表结构以 user 服务 AutoMigrate 为准。
package blogsync

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/blogtext"
	"gorm.io/gorm"
)

const (
	// DefaultCategoryName 每用户唯一默认分类名（首次同步时创建）
	DefaultCategoryName = "默认"
	maxTitle            = 200
	visPublic           = "public"
)

// Category 与 blog_categories 对齐（避免 core 依赖 user 包）
type Category struct {
	ID        uint `gorm:"primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	UserID    uint   `gorm:"column:user_id"`
	Name      string `gorm:"column:name"`
	SortOrder int    `gorm:"column:sort_order"`
	IsDefault bool   `gorm:"column:is_default"`
}

func (Category) TableName() string { return "blog_categories" }

// Article 与 blog_articles 对齐
type Article struct {
	ID                uint `gorm:"primaryKey"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
	UserID            uint       `gorm:"column:user_id"`
	Slug              string     `gorm:"column:slug"`
	Title             string     `gorm:"column:title"`
	Summary           string     `gorm:"column:summary"`
	Content           string     `gorm:"column:content"`
	CoverURL          string     `gorm:"column:cover_url"`
	Visibility        string     `gorm:"column:visibility"`
	PasswordHash      string     `gorm:"column:password_hash"`
	Recommend         bool       `gorm:"column:recommend"`
	SyncToMainProfile bool       `gorm:"column:sync_to_main_profile"`
	CategoryID        *uint      `gorm:"column:category_id"`
	ViewCount         int        `gorm:"column:view_count"`
	LikeCount         int        `gorm:"column:like_count"`
	CommentCount      int        `gorm:"column:comment_count"`
	PublishedAt       *time.Time `gorm:"column:published_at"`
	// SourceSolutionID 主站题解 id；>0 表示由题解同步生成
	SourceSolutionID *uint `gorm:"column:source_solution_id"`
	SourceProblemID  *uint `gorm:"column:source_problem_id"`
}

func (Article) TableName() string { return "blog_articles" }

// EnsureDefaultCategory 保证用户有「默认」分类，返回分类 id。
func EnsureDefaultCategory(db *gorm.DB, userID uint) (uint, error) {
	if db == nil || userID == 0 {
		return 0, fmt.Errorf("blogsync: missing db or user")
	}
	var c Category
	err := db.Where("user_id = ? AND is_default = ?", userID, true).First(&c).Error
	if err == nil {
		return c.ID, nil
	}
	if err != gorm.ErrRecordNotFound {
		// 列未迁移时可能失败；再尝试按名称「默认」兜底
		if e2 := db.Where("user_id = ? AND name = ?", userID, DefaultCategoryName).First(&c).Error; e2 == nil {
			_ = db.Model(&c).Update("is_default", true).Error
			return c.ID, nil
		}
	}
	c = Category{
		UserID:    userID,
		Name:      DefaultCategoryName,
		SortOrder: 0,
		IsDefault: true,
	}
	if err := db.Create(&c).Error; err != nil {
		// 并发：再查一次
		var again Category
		if e2 := db.Where("user_id = ? AND is_default = ?", userID, true).First(&again).Error; e2 == nil {
			return again.ID, nil
		}
		if e2 := db.Where("user_id = ? AND name = ?", userID, DefaultCategoryName).First(&again).Error; e2 == nil {
			_ = db.Model(&again).Update("is_default", true).Error
			return again.ID, nil
		}
		return 0, err
	}
	return c.ID, nil
}

// UpsertFromSolution 创建或更新题解对应的博客文章。
// articleID 为题解侧缓存的 blog_article_id（0 表示未知）。
// 公开题解自动 recommend + 同步作者组织；摘要：仅当空或仍是系统默认时按正文重生成。
// 返回 articleID、slug。
func UpsertFromSolution(db *gorm.DB, userID, solutionID, articleID uint, title, content string) (uint, string, error) {
	return UpsertFromSolutionWithProblem(db, userID, solutionID, 0, articleID, title, content)
}

// UpsertFromSolutionWithProblem same as UpsertFromSolution but stores source problem id.
func UpsertFromSolutionWithProblem(db *gorm.DB, userID, solutionID, problemID, articleID uint, title, content string) (uint, string, error) {
	if db == nil || userID == 0 || solutionID == 0 {
		return 0, "", fmt.Errorf("blogsync: missing args")
	}
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(strings.ReplaceAll(content, "\r\n", "\n"))
	if title == "" || content == "" {
		return 0, "", fmt.Errorf("blogsync: empty title/content")
	}
	if utf8.RuneCountInString(title) > maxTitle {
		runes := []rune(title)
		title = string(runes[:maxTitle])
	}
	catID, err := EnsureDefaultCategory(db, userID)
	if err != nil {
		return 0, "", err
	}
	catPtr := &catID
	sid := solutionID
	var pid *uint
	if problemID > 0 {
		pid = &problemID
	}
	slug := solutionSlug(solutionID)
	defSum := blogtext.DefaultSummary(content)

	applyUpdate := func(a *Article) (uint, string, error) {
		updates := map[string]interface{}{
			"title":                title,
			"content":              content,
			"category_id":          catPtr,
			"visibility":           visPublic,
			// 精选(recommend) 不自动设；仅同步主站资料
			"sync_to_main_profile": true,
			"source_solution_id":   &sid,
		}
		if pid != nil {
			updates["source_problem_id"] = pid
		}
		// 摘要一律按正文重算（不允许手写保留）
		updates["summary"] = defSum
		_ = db.Model(a).Updates(updates).Error
		_ = autoSurfaceOrgs(db, a.ID, userID)
		// 题解内容变更后 GC 未再引用的又拍云图（与博客保存路径一致）
		blogimg.ScheduleGCUserImages(db, userID)
		return a.ID, a.Slug, nil
	}

	// 1) 按 articleID
	if articleID > 0 {
		var a Article
		if db.Where("id = ? AND user_id = ?", articleID, userID).First(&a).Error == nil {
			return applyUpdate(&a)
		}
	}
	// 2) 按 source_solution_id
	var existing Article
	if db.Where("source_solution_id = ?", solutionID).First(&existing).Error == nil {
		return applyUpdate(&existing)
	}
	// 3) 按固定 slug solution-{id}
	if db.Where("user_id = ? AND slug = ?", userID, slug).First(&existing).Error == nil {
		return applyUpdate(&existing)
	}

	now := time.Now()
	a := Article{
		UserID:            userID,
		Slug:              slug,
		Title:             title,
		Summary:           defSum,
		Content:           content,
		Visibility:        visPublic,
		Recommend:         false,
		SyncToMainProfile: true,
		CategoryID:        catPtr,
		PublishedAt:       &now,
		SourceSolutionID:  &sid,
		SourceProblemID:   pid,
	}
	if err := db.Create(&a).Error; err != nil {
		// 并发冲突：再读一次
		if db.Where("source_solution_id = ?", solutionID).First(&existing).Error == nil {
			return applyUpdate(&existing)
		}
		if db.Where("user_id = ? AND slug = ?", userID, slug).First(&existing).Error == nil {
			return applyUpdate(&existing)
		}
		return 0, "", err
	}
	_ = autoSurfaceOrgs(db, a.ID, userID)
	// 新建镜像也可能引用已上传图；跑一次 GC 清理历史孤儿
	blogimg.ScheduleGCUserImages(db, userID)
	return a.ID, a.Slug, nil
}

// autoSurfaceOrgs writes blog_article_orgs for all author memberships (+ public domain rule).
func autoSurfaceOrgs(db *gorm.DB, articleID, userID uint) error {
	if db == nil || articleID == 0 || userID == 0 {
		return nil
	}
	var orgIDs []uint
	_ = db.Table("org_members").Where("user_id = ?", userID).Pluck("org_id", &orgIDs).Error
	// ensure public domain
	var pubID uint
	_ = db.Table("orgs").Select("id").Where("is_system = ?", true).Limit(1).Scan(&pubID).Error
	// expand: any private org also includes public domain
	seen := map[uint]struct{}{}
	var expanded []uint
	hasPrivate := false
	for _, id := range orgIDs {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		expanded = append(expanded, id)
		if pubID == 0 || id != pubID {
			hasPrivate = true
		}
	}
	if hasPrivate && pubID > 0 {
		if _, ok := seen[pubID]; !ok {
			expanded = append([]uint{pubID}, expanded...)
		}
	}
	if len(expanded) == 0 && pubID > 0 {
		expanded = []uint{pubID}
	}
	_ = db.Where("article_id = ?", articleID).Delete(&articleOrg{}).Error
	for _, oid := range expanded {
		_ = db.Exec(
			`INSERT INTO blog_article_orgs (created_at, article_id, org_id)
			 SELECT NOW(), ?, ? WHERE NOT EXISTS (
			   SELECT 1 FROM blog_article_orgs WHERE article_id = ? AND org_id = ?
			 )`,
			articleID, oid, articleID, oid,
		).Error
	}
	return nil
}

// DeleteBySolution 删除题解对应的博客文章（及组织同步行；评论/点赞由 FK 或残留可接受，尽量清）。
// 删除后对作者触发又拍云未引用图 GC。
func DeleteBySolution(db *gorm.DB, userID, solutionID, articleID uint) {
	if db == nil {
		return
	}
	var ids []uint
	ownerIDs := map[uint]struct{}{}
	if userID > 0 {
		ownerIDs[userID] = struct{}{}
	}
	if articleID > 0 {
		ids = append(ids, articleID)
		var owner uint
		_ = db.Model(&Article{}).Select("user_id").Where("id = ?", articleID).Scan(&owner).Error
		if owner > 0 {
			ownerIDs[owner] = struct{}{}
		}
	}
	if solutionID > 0 {
		var bySrc []Article
		_ = db.Select("id", "user_id").Where("source_solution_id = ?", solutionID).Find(&bySrc).Error
		for _, a := range bySrc {
			ids = append(ids, a.ID)
			if a.UserID > 0 {
				ownerIDs[a.UserID] = struct{}{}
			}
		}
		if userID > 0 {
			var bySlug Article
			if db.Select("id", "user_id").Where("user_id = ? AND slug = ?", userID, solutionSlug(solutionID)).First(&bySlug).Error == nil {
				ids = append(ids, bySlug.ID)
				if bySlug.UserID > 0 {
					ownerIDs[bySlug.UserID] = struct{}{}
				}
			}
		}
	}
	seen := map[uint]struct{}{}
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		_ = db.Where("article_id = ?", id).Delete(&articleOrg{}).Error
		_ = db.Where("article_id = ?", id).Delete(&articleComment{}).Error
		_ = db.Where("article_id = ?", id).Delete(&articleLike{}).Error
		_ = db.Where("id = ?", id).Delete(&Article{}).Error
	}
	// 题解删文后清理不再被引用的又拍云对象
	for oid := range ownerIDs {
		blogimg.ScheduleGCUserImages(db, oid)
	}
}

// LookupBySolution 按题解 id 查博客文章 id + slug。
func LookupBySolution(db *gorm.DB, solutionID uint) (id uint, slug string, ok bool) {
	if db == nil || solutionID == 0 {
		return 0, "", false
	}
	var a Article
	if db.Select("id", "slug").Where("source_solution_id = ?", solutionID).First(&a).Error == nil {
		return a.ID, a.Slug, true
	}
	return 0, "", false
}

// LookupByArticleID 按文章 id 取 slug（校验作者可选）。
func LookupByArticleID(db *gorm.DB, articleID, userID uint) (slug string, ok bool) {
	if db == nil || articleID == 0 {
		return "", false
	}
	var a Article
	q := db.Select("id", "slug", "user_id").Where("id = ?", articleID)
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	if q.First(&a).Error != nil {
		return "", false
	}
	return a.Slug, true
}

type articleOrg struct {
	ID        uint `gorm:"primaryKey"`
	ArticleID uint `gorm:"column:article_id"`
}

func (articleOrg) TableName() string { return "blog_article_orgs" }

type articleComment struct {
	ID        uint `gorm:"primaryKey"`
	ArticleID uint `gorm:"column:article_id"`
}

func (articleComment) TableName() string { return "blog_comments" }

type articleLike struct {
	ID        uint `gorm:"primaryKey"`
	ArticleID uint `gorm:"column:article_id"`
}

func (articleLike) TableName() string { return "blog_likes" }

func solutionSlug(solutionID uint) string {
	return fmt.Sprintf("solution-%d", solutionID)
}

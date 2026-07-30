package blogimg

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

const AdminImageCleanupGracePeriod = 12 * time.Hour

var (
	ErrAdminImageNotCandidate  = errors.New("admin image is not a cleanup candidate")
	ErrAdminImageSnapshotStale = errors.New("admin image cleanup snapshot is stale")
)

type AdminImageListOptions struct {
	Mode     string
	Page     int
	PageSize int
}

type AdminImageAsset struct {
	ID          uint      `json:"id"`
	UserID      uint      `json:"userId"`
	Username    string    `json:"username"`
	Name        string    `json:"name"`
	ObjectKey   string    `json:"objectKey"`
	URL         string    `json:"url"`
	ContentHash string    `json:"contentHash,omitempty"`
	Purpose     string    `json:"purpose"`
	CreatedAt   time.Time `json:"createdAt"`
	Referenced  bool      `json:"referenced"`
}

type AdminImageListResult struct {
	List         []AdminImageAsset `json:"list"`
	Total        int               `json:"total"`
	Page         int               `json:"page"`
	PageSize     int               `json:"pageSize"`
	Mode         string            `json:"mode"`
	CandidateIDs []uint            `json:"candidateIds,omitempty"`
	Snapshot     string            `json:"snapshot,omitempty"`
}

type adminImageAssetRow struct {
	ID          uint      `gorm:"primaryKey"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UserID      uint      `gorm:"column:user_id"`
	ObjectKey   string    `gorm:"column:object_key"`
	URL         string    `gorm:"column:url"`
	ContentHash string    `gorm:"column:content_hash"`
	Purpose     string    `gorm:"column:purpose"`
}

func (adminImageAssetRow) TableName() string { return "blog_image_assets" }

type adminImageUserRow struct {
	ID       uint   `gorm:"primaryKey"`
	Username string `gorm:"column:username"`
	Name     string `gorm:"column:name"`
}

func (adminImageUserRow) TableName() string { return "users" }

type adminImageReferences struct {
	hashes map[string]struct{}
	keys   map[string]struct{}
	texts  []string
}

func loadAdminImageReferences(db *gorm.DB, publicBase string) (adminImageReferences, error) {
	refs := adminImageReferences{
		hashes: map[string]struct{}{},
		keys:   map[string]struct{}{},
	}
	addHashes := func(raw string) {
		for _, hash := range DecodeImageHashes(raw) {
			refs.hashes[hash] = struct{}{}
		}
	}
	addContent := func(content, cover string) {
		refs.texts = append(refs.texts, content, cover)
		for key := range KeysFromContent(content, cover, publicBase) {
			refs.keys[key] = struct{}{}
			if hash := HashFromObjectKey(key); hash != "" {
				refs.hashes[hash] = struct{}{}
			}
		}
	}

	var articles []articleRefRow
	if err := db.Select("content", "cover_url", "image_hashes").Find(&articles).Error; err != nil {
		return refs, fmt.Errorf("query blog article image references: %w", err)
	}
	for _, article := range articles {
		addHashes(article.ImageHashes)
		addContent(article.Content, article.CoverURL)
	}

	var pages []pageRefRow
	if err := db.Select("content_md", "image_hashes").Find(&pages).Error; err != nil {
		return refs, fmt.Errorf("query blog page image references: %w", err)
	}
	for _, page := range pages {
		addHashes(page.ImageHashes)
		addContent(page.ContentMD, "")
	}
	return refs, nil
}

func (refs adminImageReferences) contains(asset adminImageAssetRow) bool {
	key := NormalizeObjectKey(asset.ObjectKey)
	hash := NormalizeHash(asset.ContentHash)
	if hash == "" {
		hash = HashFromObjectKey(key)
	}
	if hash != "" {
		if _, ok := refs.hashes[hash]; ok {
			return true
		}
	}
	if _, ok := refs.keys[key]; ok {
		return true
	}
	return AssetReferenced(key, asset.URL, refs.texts...)
}

func normalizeAdminImageListOptions(opts AdminImageListOptions) AdminImageListOptions {
	if opts.Page < 1 {
		opts.Page = 1
	}
	if opts.PageSize < 1 {
		opts.PageSize = 20
	}
	if opts.PageSize > 100 {
		opts.PageSize = 100
	}
	if opts.Mode != "cleanup" {
		opts.Mode = "all"
	}
	return opts
}

func adminImagePublicURL(publicBase string, asset adminImageAssetRow) string {
	if raw := strings.TrimSpace(asset.URL); raw != "" && !strings.HasPrefix(raw, "/") {
		return raw
	}
	key := NormalizeObjectKey(asset.ObjectKey)
	if key == "" {
		return strings.TrimSpace(asset.URL)
	}
	base := strings.TrimRight(strings.TrimSpace(publicBase), "/")
	if base == "" {
		return key
	}
	return base + key
}

func adminImageSnapshot(assets []AdminImageAsset) string {
	ordered := append([]AdminImageAsset(nil), assets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	h := sha256.New()
	for _, asset := range ordered {
		_, _ = fmt.Fprintf(h, "%d\x00%s\x00%s\x00%d\n", asset.ID, asset.ObjectKey, asset.ContentHash, asset.CreatedAt.UTC().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ListAdminImageAssetsAt(
	db *gorm.DB,
	publicBase string,
	opts AdminImageListOptions,
	now time.Time,
) (AdminImageListResult, error) {
	opts = normalizeAdminImageListOptions(opts)
	result := AdminImageListResult{
		List:     []AdminImageAsset{},
		Page:     opts.Page,
		PageSize: opts.PageSize,
		Mode:     opts.Mode,
	}
	if db == nil {
		return result, fmt.Errorf("nil database")
	}
	refs, err := loadAdminImageReferences(db, publicBase)
	if err != nil {
		return result, err
	}

	var rows []adminImageAssetRow
	if err := db.Order("id DESC").Find(&rows).Error; err != nil {
		return result, fmt.Errorf("query blog image assets: %w", err)
	}
	userIDs := make([]uint, 0)
	seenUsers := map[uint]struct{}{}
	for _, row := range rows {
		if _, ok := seenUsers[row.UserID]; !ok {
			seenUsers[row.UserID] = struct{}{}
			userIDs = append(userIDs, row.UserID)
		}
	}
	users := map[uint]adminImageUserRow{}
	if len(userIDs) > 0 {
		var userRows []adminImageUserRow
		if err := db.Where("id IN ?", userIDs).Find(&userRows).Error; err != nil {
			return result, fmt.Errorf("query blog image uploaders: %w", err)
		}
		for _, user := range userRows {
			users[user.ID] = user
		}
	}

	matched := make([]AdminImageAsset, 0, len(rows))
	for _, row := range rows {
		referenced := refs.contains(row)
		if opts.Mode == "cleanup" && (referenced || row.CreatedAt.After(now.Add(-AdminImageCleanupGracePeriod))) {
			continue
		}
		user := users[row.UserID]
		matched = append(matched, AdminImageAsset{
			ID:          row.ID,
			UserID:      row.UserID,
			Username:    user.Username,
			Name:        user.Name,
			ObjectKey:   NormalizeObjectKey(row.ObjectKey),
			URL:         adminImagePublicURL(publicBase, row),
			ContentHash: NormalizeHash(row.ContentHash),
			Purpose:     row.Purpose,
			CreatedAt:   row.CreatedAt,
			Referenced:  referenced,
		})
	}

	result.Total = len(matched)
	if opts.Mode == "cleanup" {
		result.CandidateIDs = make([]uint, 0, len(matched))
		for _, asset := range matched {
			result.CandidateIDs = append(result.CandidateIDs, asset.ID)
		}
		sort.Slice(result.CandidateIDs, func(i, j int) bool { return result.CandidateIDs[i] < result.CandidateIDs[j] })
		result.Snapshot = adminImageSnapshot(matched)
	}
	start := (opts.Page - 1) * opts.PageSize
	if start >= len(matched) {
		return result, nil
	}
	end := start + opts.PageSize
	if end > len(matched) {
		end = len(matched)
	}
	result.List = matched[start:end]
	return result, nil
}

func ListAdminImageAssets(db *gorm.DB, publicBase string, opts AdminImageListOptions) (AdminImageListResult, error) {
	return ListAdminImageAssetsAt(db, publicBase, opts, time.Now())
}

func validateAdminImageDeleter(client ObjectDeleter) error {
	if client == nil || !client.Configured() || strings.TrimSpace(client.PublicBaseURL()) == "" {
		return fmt.Errorf("blog image storage is not configured")
	}
	return nil
}

func containsAdminImageID(ids []uint, id uint) bool {
	for _, candidateID := range ids {
		if candidateID == id {
			return true
		}
	}
	return false
}

func deleteAdminImageRow(db *gorm.DB, client ObjectDeleter, id uint) error {
	var row adminImageAssetRow
	if err := db.Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrAdminImageNotCandidate
		}
		return fmt.Errorf("load blog image asset %d: %w", id, err)
	}
	key := NormalizeObjectKey(row.ObjectKey)
	if key == "" {
		return fmt.Errorf("blog image asset %d has empty object key", id)
	}
	if err := client.Delete(key); err != nil {
		return fmt.Errorf("delete remote blog image %s: %w", key, err)
	}
	res := db.Where("id = ?", id).Delete(&adminImageAssetRow{})
	if res.Error != nil {
		return fmt.Errorf("delete blog image asset %d: %w", id, res.Error)
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("delete blog image asset %d affected %d rows", id, res.RowsAffected)
	}
	return nil
}

func DeleteAdminImageAt(db *gorm.DB, client ObjectDeleter, id uint, now time.Time) (bool, error) {
	if db == nil || id == 0 {
		return false, ErrAdminImageNotCandidate
	}
	if err := validateAdminImageDeleter(client); err != nil {
		return false, err
	}
	preview, err := ListAdminImageAssetsAt(db, client.PublicBaseURL(), AdminImageListOptions{
		Mode: "cleanup", Page: 1, PageSize: 1,
	}, now)
	if err != nil {
		return false, err
	}
	if !containsAdminImageID(preview.CandidateIDs, id) {
		return false, ErrAdminImageNotCandidate
	}
	if err := deleteAdminImageRow(db, client, id); err != nil {
		return false, err
	}
	return true, nil
}

func DeleteAdminImage(db *gorm.DB, client ObjectDeleter, id uint) (bool, error) {
	return DeleteAdminImageAt(db, client, id, time.Now())
}

func sameAdminImageIDs(left, right []uint) bool {
	if len(left) != len(right) {
		return false
	}
	a := append([]uint(nil), left...)
	b := append([]uint(nil), right...)
	sort.Slice(a, func(i, j int) bool { return a[i] < a[j] })
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func DeleteAdminImagesSnapshotAt(
	db *gorm.DB,
	client ObjectDeleter,
	ids []uint,
	snapshot string,
	now time.Time,
) (int, error) {
	if db == nil || len(ids) == 0 || strings.TrimSpace(snapshot) == "" {
		return 0, ErrAdminImageSnapshotStale
	}
	if err := validateAdminImageDeleter(client); err != nil {
		return 0, err
	}
	preview, err := ListAdminImageAssetsAt(db, client.PublicBaseURL(), AdminImageListOptions{
		Mode: "cleanup", Page: 1, PageSize: 1,
	}, now)
	if err != nil {
		return 0, err
	}
	if !sameAdminImageIDs(preview.CandidateIDs, ids) ||
		subtle.ConstantTimeCompare([]byte(preview.Snapshot), []byte(strings.TrimSpace(snapshot))) != 1 {
		return 0, ErrAdminImageSnapshotStale
	}

	ordered := append([]uint(nil), ids...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	deleted := 0
	for _, id := range ordered {
		ok, err := DeleteAdminImageAt(db, client, id, now)
		if err != nil {
			return deleted, err
		}
		if ok {
			deleted++
		}
	}
	return deleted, nil
}

func DeleteAdminImagesSnapshot(db *gorm.DB, client ObjectDeleter, ids []uint, snapshot string) (int, error) {
	return DeleteAdminImagesSnapshotAt(db, client, ids, snapshot, time.Now())
}

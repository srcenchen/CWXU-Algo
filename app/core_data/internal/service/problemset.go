package service

import (
	"cwxu-algo/app/common/utils/sqllike"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/utils/auth"
	biz "cwxu-algo/app/core_data/internal/biz/service"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/userrpc"
	"cwxu-algo/api/user/v1/profile"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/registry"
	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const (
	maxProblemsetTitleRunes = 100
	maxProblemsetDescRunes  = 5000
	problemsetUnlockTTL     = 24 * time.Hour
)

// ProblemsetService 题单（收藏/待做/自定义 + 广场）
type ProblemsetService struct {
	db  *gorm.DB
	uc  *biz.ProblemUseCase
	reg *registry.Registrar
}

func NewProblemsetService(d *data.Data, uc *biz.ProblemUseCase, reg *discovery.Register) *ProblemsetService {
	var r *registry.Registrar
	if reg != nil {
		r = &reg.Reg
	}
	return &ProblemsetService{db: d.DB, uc: uc, reg: r}
}

// RegisterProblemsetRoutes 注册题单路由
func RegisterProblemsetRoutes(srv *khttp.Server, s *ProblemsetService) {
	r := srv.Route("/")
	r.GET("/v1/core/problemset/mine", s.handleMine)
	r.GET("/v1/core/problemset/square", s.handleSquare)
	r.GET("/v1/core/problemset/get", s.handleGet)
	r.GET("/v1/core/problemset/by-problem", s.handleByProblem)
	r.POST("/v1/core/problemset/create", s.handleCreate)
	r.POST("/v1/core/problemset/update", s.handleUpdate)
	r.POST("/v1/core/problemset/delete", s.handleDelete)
	r.POST("/v1/core/problemset/unlock", s.handleUnlock)
	r.POST("/v1/core/problemset/add", s.handleAdd)
	r.POST("/v1/core/problemset/add-manual", s.handleAddManual)
	r.POST("/v1/core/problemset/remove", s.handleRemove)
	r.POST("/v1/core/problemset/reorder", s.handleReorder)
	r.POST("/v1/core/problemset/like", s.handleLike)
	r.POST("/v1/core/problemset/favorite", s.handleFavorite)
	r.GET("/v1/core/problemset/favorites", s.handleFavorites)
}

// ---------- visibility helpers（可单测）----------

// CanViewProblemset 是否可读题单正文/题目列表
// unlockOK=true 表示已校验密码 unlock token
func CanViewProblemset(viewerID uint, ps *model.Problemset, unlockOK bool) bool {
	if ps == nil {
		return false
	}
	if viewerID > 0 && viewerID == ps.OwnerID {
		return true
	}
	switch ps.Visibility {
	case model.ProblemsetVisPublic:
		return true
	case model.ProblemsetVisPassword:
		return unlockOK
	default: // private
		return false
	}
}

// IsPublicProblemset 是否开放到广场 / 题目页挂出
func IsPublicProblemset(ps *model.Problemset) bool {
	return ps != nil && ps.Visibility == model.ProblemsetVisPublic && ps.Kind == model.ProblemsetKindCustom
}

func hashProblemsetPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func checkProblemsetPassword(hash, plain string) bool {
	if hash == "" || plain == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

func problemsetUnlockKey() []byte {
	h := sha256.Sum256([]byte("problemset-unlock:" + _const.JWTSecret()))
	return h[:]
}

func makeProblemsetUnlockToken(setID uint) string {
	exp := time.Now().Add(problemsetUnlockTTL).Unix()
	payload := fmt.Sprintf("%d:%d", setID, exp)
	mac := hmac.New(sha256.New, problemsetUnlockKey())
	_, _ = mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload + ":" + sig))
}

func verifyProblemsetUnlockToken(token string, setID uint) bool {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 {
		return false
	}
	id, _ := strconv.ParseUint(parts[0], 10, 64)
	exp, _ := strconv.ParseInt(parts[1], 10, 64)
	if uint(id) != setID || exp < time.Now().Unix() {
		return false
	}
	payload := parts[0] + ":" + parts[1]
	mac := hmac.New(sha256.New, problemsetUnlockKey())
	_, _ = mac.Write([]byte(payload))
	expect := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expect), []byte(parts[2]))
}

func normalizeVisibility(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case model.ProblemsetVisPublic:
		return model.ProblemsetVisPublic
	case model.ProblemsetVisPassword:
		return model.ProblemsetVisPassword
	default:
		return model.ProblemsetVisPrivate
	}
}

// ---------- handlers ----------

func (s *ProblemsetService) viewerID(ctx khttp.Context) uint {
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		return pd.UserID
	}
	return 0
}

func (s *ProblemsetService) handleMine(ctx khttp.Context) error {
	uid := s.viewerID(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	if err := dal.EnsureSystemProblemsets(context.Background(), s.db, uid); err != nil {
		log.Warnf("EnsureSystemProblemsets user=%d: %v", uid, err)
	}
	var list []model.Problemset
	if err := s.db.Where("owner_id = ?", uid).
		Order("CASE kind WHEN 'favorites' THEN 0 WHEN 'todo' THEN 1 ELSE 2 END, updated_at DESC").
		Find(&list).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加载失败"})
		return nil
	}
	setIDs := idsOfSets(list)
	liked := s.likedMap(uid, setIDs)
	favorited := s.favoritedMap(uid, setIDs)
	// 可选 problemId：标注本题是否已在各题单中（题目页「添加到题单」用）
	checkPID := queryUint(ctx, "problemId")
	contains := map[uint]bool{}
	if checkPID > 0 && len(list) > 0 {
		var hitIDs []uint
		_ = s.db.Model(&model.ProblemsetItem{}).
			Where("problem_id = ? AND problemset_id IN ?", checkPID, setIDs).
			Pluck("problemset_id", &hitIDs).Error
		for _, id := range hitIDs {
			contains[id] = true
		}
	}
	items := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		b := s.toBrief(&list[i], uid, liked[list[i].ID], favorited[list[i].ID], false)
		if checkPID > 0 {
			b["containsProblem"] = contains[list[i].ID]
		}
		items = append(items, b)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok", "data": items,
	})
	return nil
}

func (s *ProblemsetService) handleSquare(ctx khttp.Context) error {
	page, pageSize := pageParams(ctx, 1, 20, 50)
	keyword := strings.TrimSpace(ctx.Query().Get("keyword"))
	q := s.db.Model(&model.Problemset{}).
		Where("visibility = ? AND kind = ?", model.ProblemsetVisPublic, model.ProblemsetKindCustom)
	if keyword != "" {
		like := sqllike.Pattern(keyword)
		q = q.Where("title ILIKE ? OR description ILIKE ?", like, like)
	}
	var total int64
	_ = q.Count(&total).Error
	var list []model.Problemset
	if err := q.Order("like_count DESC, updated_at DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&list).Error; err != nil {
		// sqlite 无 ILIKE：降级
		if keyword != "" {
			q2 := s.db.Model(&model.Problemset{}).
				Where("visibility = ? AND kind = ?", model.ProblemsetVisPublic, model.ProblemsetKindCustom).
				Where("title LIKE ? OR description LIKE ?", sqllike.Pattern(keyword), sqllike.Pattern(keyword))
			_ = q2.Count(&total).Error
			_ = q2.Order("like_count DESC, updated_at DESC").
				Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error
		} else {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加载失败"})
			return nil
		}
	}
	uid := s.viewerID(ctx)
	setIDs := idsOfSets(list)
	liked := s.likedMap(uid, setIDs)
	favorited := s.favoritedMap(uid, setIDs)
	ownerNames := s.batchOwnerNames(ctx, list)
	items := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		b := s.toBrief(&list[i], uid, liked[list[i].ID], favorited[list[i].ID], false)
		b["ownerName"] = ownerNames[list[i].OwnerID]
		items = append(items, b)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok", "data": items,
		"total": total, "page": page, "pageSize": pageSize,
	})
	return nil
}

func (s *ProblemsetService) handleGet(ctx khttp.Context) error {
	id := queryUint(ctx, "id")
	if id == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "缺少题单 id"})
		return nil
	}
	var ps model.Problemset
	if err := s.db.First(&ps, id).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "题单不存在"})
		return nil
	}
	uid := s.viewerID(ctx)
	// 访问自己的题单时确保系统题单存在
	if uid > 0 && uid == ps.OwnerID {
		_ = dal.EnsureSystemProblemsets(context.Background(), s.db, uid)
	}
	unlockToken := strings.TrimSpace(ctx.Query().Get("unlockToken"))
	unlockOK := unlockToken != "" && verifyProblemsetUnlockToken(unlockToken, ps.ID)
	if !CanViewProblemset(uid, &ps, unlockOK) {
		if ps.Visibility == model.ProblemsetVisPassword {
			// HTTP 200 + success=false：便于前端拿到 locked 摘要（axios 对 403 会丢 body.data）
			writeJSON(ctx.Response(), 200, map[string]interface{}{
				"success": false, "message": "需要密码", "code": "PASSWORD_REQUIRED",
				"data": map[string]interface{}{
					"id": ps.ID, "title": ps.Title, "visibility": ps.Visibility,
					"ownerId": ps.OwnerID, "kind": ps.Kind, "likeCount": ps.LikeCount,
					"itemCount": ps.ItemCount, "locked": true,
				},
			})
			return nil
		}
		writeJSON(ctx.Response(), 403, map[string]interface{}{"success": false, "message": "无权查看该题单"})
		return nil
	}
	// 题目列表
	var items []model.ProblemsetItem
	_ = s.db.Where("problemset_id = ?", ps.ID).Order("sort_order ASC, id ASC").Find(&items).Error
	problemIDs := make([]uint, 0, len(items))
	for _, it := range items {
		problemIDs = append(problemIDs, it.ProblemID)
	}
	probMap := s.batchProblemsFull(problemIDs)
	statusMap := map[uint]string{}
	if uid > 0 && len(problemIDs) > 0 {
		statusMap, _ = dal.GetUserProblemStatuses(context.Background(), s.db, int64(uid), problemIDs)
	}
	outItems := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		p := probMap[it.ProblemID]
		row := map[string]interface{}{
			"id": it.ID, "problemId": it.ProblemID, "sortOrder": it.SortOrder,
			"createdAt": it.CreatedAt.Unix(),
		}
		if p != nil {
			row["title"] = p.Title
			row["platform"] = p.Platform
			row["externalId"] = p.ExternalID
			row["url"] = p.URL
			row["difficulty"] = p.Difficulty
			row["status"] = p.Status
			// 标签：题库有则带上，供题单页展示/开关
			if len(p.Tags) > 0 {
				row["tags"] = []string(p.Tags)
			} else {
				row["tags"] = []string{}
			}
		}
		if st, ok := statusMap[it.ProblemID]; ok {
			row["userStatus"] = st
		}
		outItems = append(outItems, row)
	}
	liked := s.likedMap(uid, []uint{ps.ID})
	favorited := s.favoritedMap(uid, []uint{ps.ID})
	ownerNames := s.batchOwnerNames(ctx, []model.Problemset{ps})
	data := s.toBrief(&ps, uid, liked[ps.ID], favorited[ps.ID], true)
	data["description"] = ps.Description
	data["items"] = outItems
	data["ownerName"] = ownerNames[ps.OwnerID]
	data["isOwner"] = uid > 0 && uid == ps.OwnerID
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok", "data": data,
	})
	return nil
}

func (s *ProblemsetService) handleByProblem(ctx khttp.Context) error {
	pid := queryUint(ctx, "problemId")
	if pid == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "缺少 problemId"})
		return nil
	}
	// 仅公有自定义题单
	var setIDs []uint
	_ = s.db.Model(&model.ProblemsetItem{}).
		Where("problem_id = ?", pid).
		Pluck("problemset_id", &setIDs).Error
	if len(setIDs) == 0 {
		writeJSON(ctx.Response(), 200, map[string]interface{}{
			"success": true, "message": "ok", "data": []interface{}{},
		})
		return nil
	}
	var list []model.Problemset
	_ = s.db.Where("id IN ? AND visibility = ? AND kind = ?",
		setIDs, model.ProblemsetVisPublic, model.ProblemsetKindCustom).
		Order("like_count DESC, updated_at DESC").
		Limit(20).
		Find(&list).Error
	uid := s.viewerID(ctx)
	listIDs := idsOfSets(list)
	liked := s.likedMap(uid, listIDs)
	favorited := s.favoritedMap(uid, listIDs)
	ownerNames := s.batchOwnerNames(ctx, list)
	items := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		b := s.toBrief(&list[i], uid, liked[list[i].ID], favorited[list[i].ID], false)
		b["ownerName"] = ownerNames[list[i].OwnerID]
		items = append(items, b)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok", "data": items,
	})
	return nil
}

func (s *ProblemsetService) handleCreate(ctx khttp.Context) error {
	uid := s.viewerID(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Visibility  string `json:"visibility"`
		Password    string `json:"password"`
	}
	if err := readJSONBody(ctx.Request(), &req); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "参数错误"})
		return nil
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请填写题单标题"})
		return nil
	}
	if utf8.RuneCountInString(title) > maxProblemsetTitleRunes {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "标题过长"})
		return nil
	}
	desc := strings.TrimSpace(req.Description)
	if utf8.RuneCountInString(desc) > maxProblemsetDescRunes {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "描述过长"})
		return nil
	}
	vis := normalizeVisibility(req.Visibility)
	row := model.Problemset{
		OwnerID:     uid,
		Title:       title,
		Description: desc,
		Kind:        model.ProblemsetKindCustom,
		Visibility:  vis,
	}
	if vis == model.ProblemsetVisPassword {
		pw := strings.TrimSpace(req.Password)
		if pw == "" {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请设置访问密码"})
			return nil
		}
		hash, err := hashProblemsetPassword(pw)
		if err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "密码处理失败"})
			return nil
		}
		row.PasswordHash = hash
	}
	if err := s.db.Create(&row).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "创建失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok", "data": s.toBrief(&row, uid, false, false, true),
	})
	return nil
}

func (s *ProblemsetService) handleUpdate(ctx khttp.Context) error {
	uid := s.viewerID(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		ID            uint   `json:"id"`
		Title         string `json:"title"`
		Description   string `json:"description"`
		Visibility    string `json:"visibility"`
		Password      string `json:"password"`
		ClearPassword bool   `json:"clearPassword"`
	}
	if err := readJSONBody(ctx.Request(), &req); err != nil || req.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "参数错误"})
		return nil
	}
	var ps model.Problemset
	if err := s.db.First(&ps, req.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "题单不存在"})
		return nil
	}
	if ps.OwnerID != uid {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"success": false, "message": "只能修改自己的题单"})
		return nil
	}
	updates := map[string]interface{}{}
	if t := strings.TrimSpace(req.Title); t != "" {
		if utf8.RuneCountInString(t) > maxProblemsetTitleRunes {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "标题过长"})
			return nil
		}
		// 系统题单标题固定
		if ps.Kind == model.ProblemsetKindCustom {
			updates["title"] = t
		}
	}
	if req.Description != "" || ctx.Request().ContentLength > 0 {
		// 允许清空描述：前端始终传 description 字段
		desc := strings.TrimSpace(req.Description)
		if utf8.RuneCountInString(desc) > maxProblemsetDescRunes {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "描述过长"})
			return nil
		}
		updates["description"] = desc
	}
	// 系统题单强制 private
	if ps.Kind != model.ProblemsetKindCustom {
		// 只允许改描述
	} else if v := strings.TrimSpace(req.Visibility); v != "" {
		vis := normalizeVisibility(v)
		updates["visibility"] = vis
		if vis == model.ProblemsetVisPassword {
			pw := strings.TrimSpace(req.Password)
			if pw != "" {
				hash, err := hashProblemsetPassword(pw)
				if err != nil {
					writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "密码处理失败"})
					return nil
				}
				updates["password_hash"] = hash
			} else if ps.PasswordHash == "" {
				writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请设置访问密码"})
				return nil
			}
		} else {
			if req.ClearPassword || vis != model.ProblemsetVisPassword {
				updates["password_hash"] = ""
			}
		}
	}
	if len(updates) > 0 {
		if err := s.db.Model(&ps).Updates(updates).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "更新失败"})
			return nil
		}
		_ = s.db.First(&ps, ps.ID)
	}
	liked := s.likedMap(uid, []uint{ps.ID})
	favorited := s.favoritedMap(uid, []uint{ps.ID})
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok", "data": s.toBrief(&ps, uid, liked[ps.ID], favorited[ps.ID], true),
	})
	return nil
}

func (s *ProblemsetService) handleDelete(ctx khttp.Context) error {
	uid := s.viewerID(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		ID uint `json:"id"`
	}
	if err := readJSONBody(ctx.Request(), &req); err != nil || req.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "参数错误"})
		return nil
	}
	var ps model.Problemset
	if err := s.db.First(&ps, req.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "题单不存在"})
		return nil
	}
	if ps.OwnerID != uid {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"success": false, "message": "只能删除自己的题单"})
		return nil
	}
	if ps.Kind != model.ProblemsetKindCustom {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "系统题单不可删除"})
		return nil
	}
	_ = s.db.Where("problemset_id = ?", ps.ID).Delete(&model.ProblemsetItem{}).Error
	_ = s.db.Where("problemset_id = ?", ps.ID).Delete(&model.ProblemsetLike{}).Error
	if err := s.db.Delete(&ps).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "删除失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"success": true, "message": "ok"})
	return nil
}

func (s *ProblemsetService) handleUnlock(ctx khttp.Context) error {
	var req struct {
		ID       uint   `json:"id"`
		Password string `json:"password"`
	}
	if err := readJSONBody(ctx.Request(), &req); err != nil || req.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "参数错误"})
		return nil
	}
	var ps model.Problemset
	if err := s.db.First(&ps, req.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "题单不存在"})
		return nil
	}
	if ps.Visibility != model.ProblemsetVisPassword {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "该题单无需密码"})
		return nil
	}
	if !checkProblemsetPassword(ps.PasswordHash, strings.TrimSpace(req.Password)) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"success": false, "message": "密码错误"})
		return nil
	}
	token := makeProblemsetUnlockToken(ps.ID)
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok",
		"data": map[string]interface{}{"unlockToken": token, "expiresIn": int(problemsetUnlockTTL.Seconds())},
	})
	return nil
}

func (s *ProblemsetService) handleAdd(ctx khttp.Context) error {
	uid := s.viewerID(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		// ProblemsetID 可选：0 表示仅向题库入库，不加入题单
		ProblemsetID uint   `json:"problemsetId"`
		ProblemID    uint   `json:"problemId"`
		URL          string `json:"url"`
	}
	if err := readJSONBody(ctx.Request(), &req); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "参数错误"})
		return nil
	}
	// 仅按 problemId 加入题单时必须带 problemsetId；按 url 入库时 problemsetId 可省略
	if req.ProblemsetID == 0 && req.ProblemID > 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请提供题单 id"})
		return nil
	}
	var ps *model.Problemset
	if req.ProblemsetID > 0 {
		var row model.Problemset
		if err := s.db.First(&row, req.ProblemsetID).Error; err != nil {
			writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "题单不存在"})
			return nil
		}
		if row.OwnerID != uid {
			writeJSON(ctx.Response(), 403, map[string]interface{}{"success": false, "message": "只能向自己的题单加题"})
			return nil
		}
		ps = &row
	}

	var problemID uint
	fetchTriggered := false
	if req.ProblemID > 0 {
		var p model.Problem
		if err := s.db.First(&p, req.ProblemID).Error; err != nil {
			writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "题目不存在"})
			return nil
		}
		problemID = p.ID
		if s.uc != nil {
			needFetch := strings.TrimSpace(p.ContentMD) == "" || biz.ContentLooksBroken(p.ContentMD)
			if err := s.uc.ForceEnqueueFetch(p.ID, uid); err != nil {
				log.Warnf("problemset add force fetch id=%d: %v", p.ID, err)
			} else if needFetch {
				fetchTriggered = true
			}
		}
	} else if u := strings.TrimSpace(req.URL); u != "" {
		parsed, err := biz.ParseProblemURL(u)
		if err != nil {
			// 200 + success=false：前端 axios 可拿到 code，引导手动加题
			writeJSON(ctx.Response(), 200, map[string]interface{}{
				"success": false, "message": "无法识别该题目链接", "code": "URL_PARSE_FAILED",
			})
			return nil
		}
		if s.uc == nil {
			// 无 usecase：仅查库或建空记录
			var existing model.Problem
			err := s.db.Where("platform = ? AND external_id = ?", parsed.Platform, parsed.ExternalID).First(&existing).Error
			if err == gorm.ErrRecordNotFound {
				existing = model.Problem{
					Platform: parsed.Platform, ExternalID: parsed.ExternalID,
					Title: parsed.Title, URL: parsed.URL, Status: model.ProblemStatusPending,
					Tags: model.StringArray{},
				}
				if err := s.db.Create(&existing).Error; err != nil {
					writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "入库失败"})
					return nil
				}
			} else if err != nil {
				writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "查询题目失败"})
				return nil
			}
			problemID = existing.ID
		} else {
			p, err := s.uc.UpsertProblemFromParsedForUser(parsed, uid)
			if err != nil || p == nil {
				writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "题目处理失败"})
				return nil
			}
			problemID = p.ID
			// 空题面或损坏题面都会触发后台最高优先级补爬（Upsert 内 ForceEnqueueFetch）
			fetchTriggered = strings.TrimSpace(p.ContentMD) == "" || biz.ContentLooksBroken(p.ContentMD)
		}
	} else {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "请提供题目 id 或链接"})
		return nil
	}

	if ps != nil {
		if err := s.linkProblemToSet(ps.ID, problemID); err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加入失败"})
			return nil
		}
	}
	// 回填识别摘要：前端 5s 内确认弹窗「是否为某平台某题」
	platform, title, externalID := "", "", ""
	if problemID > 0 {
		var p model.Problem
		if s.db.Select("id", "platform", "title", "external_id").First(&p, problemID).Error == nil {
			platform = p.Platform
			title = p.Title
			externalID = p.ExternalID
		}
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok",
		"data": map[string]interface{}{
			"problemId":      problemID,
			"fetchTriggered": fetchTriggered,
			"platform":       platform,
			"title":          title,
			"externalId":     externalID,
		},
	})
	return nil
}

// handleAddManual 链接无法识别时：用户手动建题；可选加入题单（无需审核）
// problemsetId 为 0 时仅向题库入库。
func (s *ProblemsetService) handleAddManual(ctx khttp.Context) error {
	uid := s.viewerID(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		// ProblemsetID 可选：0 表示仅向题库入库
		ProblemsetID uint     `json:"problemsetId"`
		Title        string   `json:"title"`
		ContentMD    string   `json:"contentMd"`
		Tags         []string `json:"tags"`
		SourceURL    string   `json:"sourceUrl"`
	}
	if err := readJSONBody(ctx.Request(), &req); err != nil {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "参数错误"})
		return nil
	}
	var ps *model.Problemset
	if req.ProblemsetID > 0 {
		var row model.Problemset
		if err := s.db.First(&row, req.ProblemsetID).Error; err != nil {
			writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "题单不存在"})
			return nil
		}
		if row.OwnerID != uid {
			writeJSON(ctx.Response(), 403, map[string]interface{}{"success": false, "message": "只能向自己的题单加题"})
			return nil
		}
		ps = &row
	}
	if s.uc == nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "服务未就绪"})
		return nil
	}
	p, err := s.uc.CreateManualProblem(uid, req.Title, req.ContentMD, req.SourceURL, req.Tags)
	if err != nil || p == nil {
		msg := "创建题目失败"
		if err != nil {
			msg = err.Error()
		}
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": msg})
		return nil
	}
	if ps != nil {
		if err := s.linkProblemToSet(ps.ID, p.ID); err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加入题单失败"})
			return nil
		}
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok",
		"data": map[string]interface{}{"problemId": p.ID, "fetchTriggered": false},
	})
	return nil
}

// linkProblemToSet 幂等将题目加入题单
func (s *ProblemsetService) linkProblemToSet(setID, problemID uint) error {
	var n int64
	_ = s.db.Model(&model.ProblemsetItem{}).
		Where("problemset_id = ? AND problem_id = ?", setID, problemID).
		Count(&n).Error
	if n > 0 {
		return nil
	}
	var maxSort int
	_ = s.db.Model(&model.ProblemsetItem{}).
		Where("problemset_id = ?", setID).
		Select("COALESCE(MAX(sort_order),0)").Scan(&maxSort).Error
	item := model.ProblemsetItem{
		ProblemsetID: setID,
		ProblemID:    problemID,
		SortOrder:    maxSort + 1,
	}
	if err := s.db.Create(&item).Error; err != nil {
		return err
	}
	_ = s.db.Model(&model.Problemset{}).Where("id = ?", setID).
		UpdateColumn("item_count", gorm.Expr("item_count + 1")).Error
	return nil
}

func (s *ProblemsetService) handleRemove(ctx khttp.Context) error {
	uid := s.viewerID(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		ProblemsetID uint `json:"problemsetId"`
		ProblemID    uint `json:"problemId"`
	}
	if err := readJSONBody(ctx.Request(), &req); err != nil || req.ProblemsetID == 0 || req.ProblemID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "参数错误"})
		return nil
	}
	var ps model.Problemset
	if err := s.db.First(&ps, req.ProblemsetID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "题单不存在"})
		return nil
	}
	if ps.OwnerID != uid {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"success": false, "message": "只能修改自己的题单"})
		return nil
	}
	res := s.db.Where("problemset_id = ? AND problem_id = ?", ps.ID, req.ProblemID).
		Delete(&model.ProblemsetItem{})
	if res.Error != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "移除失败"})
		return nil
	}
	if res.RowsAffected > 0 {
		_ = s.db.Model(&model.Problemset{}).
			Where("id = ? AND item_count > 0", ps.ID).
			UpdateColumn("item_count", gorm.Expr("item_count - 1")).Error
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"success": true, "message": "ok"})
	return nil
}

// handleReorder 拖拽排序：按 ids（题单项 id）顺序重写 sort_order 为 0,1,2…
func (s *ProblemsetService) handleReorder(ctx khttp.Context) error {
	uid := s.viewerID(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		ProblemsetID uint   `json:"problemsetId"`
		IDs          []uint `json:"ids"`
	}
	if err := readJSONBody(ctx.Request(), &req); err != nil || req.ProblemsetID == 0 || len(req.IDs) == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "参数错误"})
		return nil
	}
	// 去重保序
	seen := make(map[uint]struct{}, len(req.IDs))
	ids := make([]uint, 0, len(req.IDs))
	for _, id := range req.IDs {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "顺序列表不能为空"})
		return nil
	}
	var ps model.Problemset
	if err := s.db.First(&ps, req.ProblemsetID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "题单不存在"})
		return nil
	}
	if ps.OwnerID != uid {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"success": false, "message": "只能修改自己的题单"})
		return nil
	}
	// 校验 ids 均属该题单，且覆盖当前全部题单项（防止半量乱序）
	var existing []model.ProblemsetItem
	if err := s.db.Where("problemset_id = ?", ps.ID).Find(&existing).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "读取题单项失败"})
		return nil
	}
	existSet := make(map[uint]struct{}, len(existing))
	for _, it := range existing {
		existSet[it.ID] = struct{}{}
	}
	if len(ids) != len(existSet) {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "顺序列表与题单项不一致"})
		return nil
	}
	for _, id := range ids {
		if _, ok := existSet[id]; !ok {
			writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "存在不属于该题单的项"})
			return nil
		}
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		// 单条 VALUES join 批量更新排序，替代逐项 UPDATE
		var sb strings.Builder
		args := make([]interface{}, 0, len(ids)*2+1)
		sb.WriteString(`
			UPDATE problemset_items AS it
			SET sort_order = v.sort_order
			FROM (VALUES `)
		for i, id := range ids {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?::bigint, ?::bigint)")
			args = append(args, id, i)
		}
		sb.WriteString(`) AS v(id, sort_order)
			WHERE it.id = v.id AND it.problemset_id = ?`)
		args = append(args, ps.ID)
		if err := tx.Exec(sb.String(), args...).Error; err != nil {
			return err
		}
		return tx.Model(&model.Problemset{}).Where("id = ?", ps.ID).
			UpdateColumn("updated_at", time.Now()).Error
	})
	if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "排序保存失败"})
		return nil
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{"success": true, "message": "ok"})
	return nil
}

func (s *ProblemsetService) handleLike(ctx khttp.Context) error {
	uid := s.viewerID(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		ID uint `json:"id"`
	}
	if err := readJSONBody(ctx.Request(), &req); err != nil || req.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "参数错误"})
		return nil
	}
	var ps model.Problemset
	if err := s.db.First(&ps, req.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "题单不存在"})
		return nil
	}
	// 仅公有题单可点赞；所有者也可赞自己
	if ps.Visibility != model.ProblemsetVisPublic && ps.OwnerID != uid {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"success": false, "message": "该题单不可点赞"})
		return nil
	}
	var existing model.ProblemsetLike
	err := s.db.Where("user_id = ? AND problemset_id = ?", uid, ps.ID).First(&existing).Error
	liked := false
	if err == gorm.ErrRecordNotFound {
		if err := s.db.Create(&model.ProblemsetLike{UserID: uid, ProblemsetID: ps.ID}).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "点赞失败"})
			return nil
		}
		_ = s.db.Model(&model.Problemset{}).Where("id = ?", ps.ID).
			UpdateColumn("like_count", gorm.Expr("like_count + 1")).Error
		liked = true
	} else if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "点赞失败"})
		return nil
	} else {
		_ = s.db.Delete(&existing).Error
		_ = s.db.Model(&model.Problemset{}).Where("id = ? AND like_count > 0", ps.ID).
			UpdateColumn("like_count", gorm.Expr("like_count - 1")).Error
		liked = false
	}
	var likeCount int
	_ = s.db.Model(&model.Problemset{}).Select("like_count").Where("id = ?", ps.ID).Scan(&likeCount).Error
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok",
		"data": map[string]interface{}{"liked": liked, "likeCount": likeCount},
	})
	return nil
}

// handleFavorite 切换收藏（与点赞分离；仅公有自定义题单）
func (s *ProblemsetService) handleFavorite(ctx khttp.Context) error {
	uid := s.viewerID(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	var req struct {
		ID uint `json:"id"`
	}
	if err := readJSONBody(ctx.Request(), &req); err != nil || req.ID == 0 {
		writeJSON(ctx.Response(), 400, map[string]interface{}{"success": false, "message": "参数错误"})
		return nil
	}
	var ps model.Problemset
	if err := s.db.First(&ps, req.ID).Error; err != nil {
		writeJSON(ctx.Response(), 404, map[string]interface{}{"success": false, "message": "题单不存在"})
		return nil
	}
	// 仅公有自定义题单可收藏（广场场景）；系统题单不可
	if !IsPublicProblemset(&ps) {
		writeJSON(ctx.Response(), 403, map[string]interface{}{"success": false, "message": "仅公开题单可收藏"})
		return nil
	}
	var existing model.ProblemsetFavorite
	err := s.db.Where("user_id = ? AND problemset_id = ?", uid, ps.ID).First(&existing).Error
	favorited := false
	if err == gorm.ErrRecordNotFound {
		if err := s.db.Create(&model.ProblemsetFavorite{UserID: uid, ProblemsetID: ps.ID}).Error; err != nil {
			writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "收藏失败"})
			return nil
		}
		favorited = true
	} else if err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "收藏失败"})
		return nil
	} else {
		_ = s.db.Delete(&existing).Error
		favorited = false
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok",
		"data": map[string]interface{}{"favorited": favorited},
	})
	return nil
}

// handleFavorites 我收藏的题单（排除自己的）
func (s *ProblemsetService) handleFavorites(ctx khttp.Context) error {
	uid := s.viewerID(ctx)
	if uid == 0 {
		writeJSON(ctx.Response(), 401, map[string]interface{}{"success": false, "message": "请先登录"})
		return nil
	}
	page, pageSize := pageParams(ctx, 1, 20, 50)
	// 收藏表 join 题单：他人 + 仍 public custom
	var total int64
	base := s.db.Table("problemset_favorites AS f").
		Joins("INNER JOIN problemsets AS p ON p.id = f.problemset_id").
		Where("f.user_id = ?", uid).
		Where("p.owner_id <> ?", uid).
		Where("p.visibility = ? AND p.kind = ?", model.ProblemsetVisPublic, model.ProblemsetKindCustom)
	_ = base.Count(&total).Error
	var list []model.Problemset
	if err := s.db.Table("problemsets AS p").
		Select("p.*").
		Joins("INNER JOIN problemset_favorites AS f ON f.problemset_id = p.id").
		Where("f.user_id = ?", uid).
		Where("p.owner_id <> ?", uid).
		Where("p.visibility = ? AND p.kind = ?", model.ProblemsetVisPublic, model.ProblemsetKindCustom).
		Order("f.created_at DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&list).Error; err != nil {
		writeJSON(ctx.Response(), 500, map[string]interface{}{"success": false, "message": "加载失败"})
		return nil
	}
	setIDs := idsOfSets(list)
	liked := s.likedMap(uid, setIDs)
	ownerNames := s.batchOwnerNames(ctx, list)
	items := make([]map[string]interface{}, 0, len(list))
	for i := range list {
		b := s.toBrief(&list[i], uid, liked[list[i].ID], true, false)
		b["ownerName"] = ownerNames[list[i].OwnerID]
		items = append(items, b)
	}
	writeJSON(ctx.Response(), 200, map[string]interface{}{
		"success": true, "message": "ok", "data": items,
		"total": total, "page": page, "pageSize": pageSize,
	})
	return nil
}

// ---------- serializers / helpers ----------

func (s *ProblemsetService) toBrief(ps *model.Problemset, viewerID uint, liked, favorited, withDesc bool) map[string]interface{} {
	m := map[string]interface{}{
		"id":          ps.ID,
		"ownerId":     ps.OwnerID,
		"title":       ps.Title,
		"kind":        ps.Kind,
		"visibility":  ps.Visibility,
		"likeCount":   ps.LikeCount,
		"itemCount":   ps.ItemCount,
		"liked":       liked,
		"favorited":   favorited,
		"isOwner":     viewerID > 0 && viewerID == ps.OwnerID,
		"createdAt":   ps.CreatedAt.Unix(),
		"updatedAt":   ps.UpdatedAt.Unix(),
		"isSystem":    ps.Kind == model.ProblemsetKindFavorites || ps.Kind == model.ProblemsetKindTodo,
	}
	if withDesc {
		m["description"] = ps.Description
	}
	return m
}

func idsOfSets(list []model.Problemset) []uint {
	out := make([]uint, 0, len(list))
	for _, p := range list {
		out = append(out, p.ID)
	}
	return out
}

func (s *ProblemsetService) likedMap(userID uint, setIDs []uint) map[uint]bool {
	out := map[uint]bool{}
	if userID == 0 || len(setIDs) == 0 {
		return out
	}
	var rows []model.ProblemsetLike
	_ = s.db.Where("user_id = ? AND problemset_id IN ?", userID, setIDs).Find(&rows).Error
	for _, r := range rows {
		out[r.ProblemsetID] = true
	}
	return out
}

func (s *ProblemsetService) favoritedMap(userID uint, setIDs []uint) map[uint]bool {
	out := map[uint]bool{}
	if userID == 0 || len(setIDs) == 0 {
		return out
	}
	var rows []model.ProblemsetFavorite
	_ = s.db.Where("user_id = ? AND problemset_id IN ?", userID, setIDs).Find(&rows).Error
	for _, r := range rows {
		out[r.ProblemsetID] = true
	}
	return out
}

func (s *ProblemsetService) batchProblemsFull(ids []uint) map[uint]*model.Problem {
	out := map[uint]*model.Problem{}
	if len(ids) == 0 {
		return out
	}
	var list []model.Problem
	_ = s.db.Where("id IN ?", ids).Find(&list).Error
	for i := range list {
		p := list[i]
		out[p.ID] = &p
	}
	return out
}

func (s *ProblemsetService) batchOwnerNames(ctx khttp.Context, list []model.Problemset) map[uint]string {
	out := map[uint]string{}
	if len(list) == 0 || s.reg == nil {
		return out
	}
	seen := map[uint]struct{}{}
	ids := make([]int64, 0)
	for _, p := range list {
		if _, ok := seen[p.OwnerID]; ok {
			continue
		}
		seen[p.OwnerID] = struct{}{}
		ids = append(ids, int64(p.OwnerID))
	}
	client, err := userrpc.ProfileClient(s.reg)
	if err != nil {
		return out
	}
	var orgID int64
	if pd := auth.GetCurrentUser(ctx); pd != nil {
		orgID = int64(pd.OrgID)
	}
	res, err := client.GetByIds(context.Background(), &profile.GetByIdsReq{UserIds: ids, OrgId: orgID})
	if err != nil || res == nil {
		return out
	}
	for _, u := range res.Profiles {
		if u == nil {
			continue
		}
		name := u.Name
		if name == "" {
			name = u.Username
		}
		out[uint(u.UserId)] = name
	}
	return out
}

// silence unused import if rand unused in some builds
var _ = rand.Read
var _ = http.StatusOK

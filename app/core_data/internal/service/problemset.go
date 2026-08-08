package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	pb "cwxu-algo/api/core/v1/problemset"
	"cwxu-algo/api/user/v1/profile"
	_const "cwxu-algo/app/common/const"
	"cwxu-algo/app/common/discovery"
	"cwxu-algo/app/common/utils/auth"
	"cwxu-algo/app/common/utils/sqllike"
	biz "cwxu-algo/app/core_data/internal/biz/service"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/userrpc"

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
// 实现 proto：api/core/v1/problemset/problemset.proto（ProblemsetHTTPServer）。
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

// ---------- proto handlers ----------

func (s *ProblemsetService) Mine(ctx context.Context, req *pb.MineReq) (*pb.MineRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.MineRes{Success: false, Message: "请先登录"}, nil
	}
	if err := dal.EnsureSystemProblemsets(ctx, s.db, uid); err != nil {
		log.Warnf("EnsureSystemProblemsets user=%d: %v", uid, err)
	}
	var list []model.Problemset
	if err := s.db.WithContext(ctx).Where("owner_id = ?", uid).
		Order("CASE kind WHEN 'favorites' THEN 0 WHEN 'todo' THEN 1 ELSE 2 END, updated_at DESC").
		Find(&list).Error; err != nil {
		return &pb.MineRes{Success: false, Message: "加载失败"}, nil
	}
	setIDs := idsOfSets(list)
	liked := s.likedMap(ctx, uid, setIDs)
	favorited := s.favoritedMap(ctx, uid, setIDs)
	// 可选 problemId：标注本题是否已在各题单中（题目页「添加到题单」用）
	checkPID := uint(req.ProblemId)
	contains := map[uint]bool{}
	if checkPID > 0 && len(list) > 0 {
		var hitIDs []uint
		_ = s.db.WithContext(ctx).Model(&model.ProblemsetItem{}).
			Where("problem_id = ? AND problemset_id IN ?", checkPID, setIDs).
			Pluck("problemset_id", &hitIDs).Error
		for _, id := range hitIDs {
			contains[id] = true
		}
	}
	items := make([]*pb.ProblemsetInfo, 0, len(list))
	for i := range list {
		b := s.toBrief(&list[i], uid, liked[list[i].ID], favorited[list[i].ID], false)
		if checkPID > 0 {
			b.ContainsProblem = contains[list[i].ID]
		}
		items = append(items, b)
	}
	return &pb.MineRes{Success: true, Message: "ok", Data: items}, nil
}

func (s *ProblemsetService) Square(ctx context.Context, req *pb.SquareReq) (*pb.SquareRes, error) {
	page := int(req.Page)
	if page <= 0 {
		page = 1
	}
	pageSize := int(req.PageSize)
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 50 {
		pageSize = 50
	}
	keyword := strings.TrimSpace(req.Keyword)
	q := s.db.WithContext(ctx).Model(&model.Problemset{}).
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
			q2 := s.db.WithContext(ctx).Model(&model.Problemset{}).
				Where("visibility = ? AND kind = ?", model.ProblemsetVisPublic, model.ProblemsetKindCustom).
				Where("title LIKE ? OR description LIKE ?", sqllike.Pattern(keyword), sqllike.Pattern(keyword))
			_ = q2.Count(&total).Error
			_ = q2.Order("like_count DESC, updated_at DESC").
				Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error
		} else {
			return &pb.SquareRes{Success: false, Message: "加载失败"}, nil
		}
	}
	uid := auth.GetCurrentUserId(ctx)
	setIDs := idsOfSets(list)
	liked := s.likedMap(ctx, uid, setIDs)
	favorited := s.favoritedMap(ctx, uid, setIDs)
	ownerNames := s.batchOwnerNames(ctx, list)
	items := make([]*pb.ProblemsetInfo, 0, len(list))
	for i := range list {
		b := s.toBrief(&list[i], uid, liked[list[i].ID], favorited[list[i].ID], false)
		b.OwnerName = ownerNames[list[i].OwnerID]
		items = append(items, b)
	}
	return &pb.SquareRes{
		Success: true, Message: "ok", Data: items,
		Total: total, Page: int64(page), PageSize: int64(pageSize),
	}, nil
}

func (s *ProblemsetService) Get(ctx context.Context, req *pb.GetReq) (*pb.GetRes, error) {
	id := uint(req.Id)
	if id == 0 {
		return &pb.GetRes{Success: false, Message: "缺少题单 id"}, nil
	}
	var ps model.Problemset
	if err := s.db.WithContext(ctx).First(&ps, id).Error; err != nil {
		return &pb.GetRes{Success: false, Message: "题单不存在"}, nil
	}
	uid := auth.GetCurrentUserId(ctx)
	// 访问自己的题单时确保系统题单存在
	if uid > 0 && uid == ps.OwnerID {
		_ = dal.EnsureSystemProblemsets(ctx, s.db, uid)
	}
	unlockToken := strings.TrimSpace(req.UnlockToken)
	unlockOK := unlockToken != "" && verifyProblemsetUnlockToken(unlockToken, ps.ID)
	if !CanViewProblemset(uid, &ps, unlockOK) {
		if ps.Visibility == model.ProblemsetVisPassword {
			// HTTP 200 + success=false：便于前端拿到 locked 摘要（axios 对 403 会丢 body.data）
			return &pb.GetRes{
				Success: false, Message: "需要密码", Code: "PASSWORD_REQUIRED",
				Data: &pb.ProblemsetInfo{
					Id: int64(ps.ID), Title: ps.Title, Visibility: ps.Visibility,
					OwnerId: int64(ps.OwnerID), Kind: ps.Kind, LikeCount: int32(ps.LikeCount),
					ItemCount: int32(ps.ItemCount), Locked: true,
				},
			}, nil
		}
		return &pb.GetRes{Success: false, Message: "无权查看该题单"}, nil
	}
	// 题目列表
	var items []model.ProblemsetItem
	_ = s.db.WithContext(ctx).Where("problemset_id = ?", ps.ID).Order("sort_order ASC, id ASC").Find(&items).Error
	problemIDs := make([]uint, 0, len(items))
	for _, it := range items {
		problemIDs = append(problemIDs, it.ProblemID)
	}
	probMap := s.batchProblemsFull(ctx, problemIDs)
	statusMap := map[uint]string{}
	if uid > 0 && len(problemIDs) > 0 {
		statusMap, _ = dal.GetUserProblemStatuses(ctx, s.db, int64(uid), problemIDs)
	}
	outItems := make([]*pb.ProblemsetItem, 0, len(items))
	for _, it := range items {
		p := probMap[it.ProblemID]
		row := &pb.ProblemsetItem{
			Id:        int64(it.ID),
			ProblemId: int64(it.ProblemID),
			SortOrder: int32(it.SortOrder),
			CreatedAt: it.CreatedAt.Unix(),
		}
		if p != nil {
			row.Title = p.Title
			row.Platform = p.Platform
			row.ExternalId = p.ExternalID
			row.Url = p.URL
			row.Difficulty = p.Difficulty
			row.Status = p.Status
			// 标签：题库有则带上，供题单页展示/开关
			row.Tags = []string(p.Tags)
		}
		if st, ok := statusMap[it.ProblemID]; ok {
			row.UserStatus = st
		}
		outItems = append(outItems, row)
	}
	liked := s.likedMap(ctx, uid, []uint{ps.ID})
	favorited := s.favoritedMap(ctx, uid, []uint{ps.ID})
	ownerNames := s.batchOwnerNames(ctx, []model.Problemset{ps})
	data := s.toBrief(&ps, uid, liked[ps.ID], favorited[ps.ID], true)
	data.Description = ps.Description
	data.Items = outItems
	data.OwnerName = ownerNames[ps.OwnerID]
	data.IsOwner = uid > 0 && uid == ps.OwnerID
	return &pb.GetRes{Success: true, Message: "ok", Data: data}, nil
}

func (s *ProblemsetService) ByProblem(ctx context.Context, req *pb.ByProblemReq) (*pb.ByProblemRes, error) {
	pid := uint(req.ProblemId)
	if pid == 0 {
		return &pb.ByProblemRes{Success: false, Message: "缺少 problemId"}, nil
	}
	// 仅公有自定义题单
	var setIDs []uint
	_ = s.db.WithContext(ctx).Model(&model.ProblemsetItem{}).
		Where("problem_id = ?", pid).
		Pluck("problemset_id", &setIDs).Error
	if len(setIDs) == 0 {
		return &pb.ByProblemRes{Success: true, Message: "ok", Data: []*pb.ProblemsetInfo{}}, nil
	}
	var list []model.Problemset
	_ = s.db.WithContext(ctx).Where("id IN ? AND visibility = ? AND kind = ?",
		setIDs, model.ProblemsetVisPublic, model.ProblemsetKindCustom).
		Order("like_count DESC, updated_at DESC").
		Limit(20).
		Find(&list).Error
	uid := auth.GetCurrentUserId(ctx)
	listIDs := idsOfSets(list)
	liked := s.likedMap(ctx, uid, listIDs)
	favorited := s.favoritedMap(ctx, uid, listIDs)
	ownerNames := s.batchOwnerNames(ctx, list)
	items := make([]*pb.ProblemsetInfo, 0, len(list))
	for i := range list {
		b := s.toBrief(&list[i], uid, liked[list[i].ID], favorited[list[i].ID], false)
		b.OwnerName = ownerNames[list[i].OwnerID]
		items = append(items, b)
	}
	return &pb.ByProblemRes{Success: true, Message: "ok", Data: items}, nil
}

func (s *ProblemsetService) Create(ctx context.Context, req *pb.CreateReq) (*pb.CreateRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.CreateRes{Success: false, Message: "请先登录"}, nil
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return &pb.CreateRes{Success: false, Message: "请填写题单标题"}, nil
	}
	if utf8.RuneCountInString(title) > maxProblemsetTitleRunes {
		return &pb.CreateRes{Success: false, Message: "标题过长"}, nil
	}
	desc := strings.TrimSpace(req.Description)
	if utf8.RuneCountInString(desc) > maxProblemsetDescRunes {
		return &pb.CreateRes{Success: false, Message: "描述过长"}, nil
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
			return &pb.CreateRes{Success: false, Message: "请设置访问密码"}, nil
		}
		hash, err := hashProblemsetPassword(pw)
		if err != nil {
			return &pb.CreateRes{Success: false, Message: "密码处理失败"}, nil
		}
		row.PasswordHash = hash
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return &pb.CreateRes{Success: false, Message: "创建失败"}, nil
	}
	return &pb.CreateRes{Success: true, Message: "ok", Data: s.toBrief(&row, uid, false, false, true)}, nil
}

func (s *ProblemsetService) Update(ctx context.Context, req *pb.UpdateReq) (*pb.UpdateRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.UpdateRes{Success: false, Message: "请先登录"}, nil
	}
	id := uint(req.Id)
	if id == 0 {
		return &pb.UpdateRes{Success: false, Message: "参数错误"}, nil
	}
	var ps model.Problemset
	if err := s.db.WithContext(ctx).First(&ps, id).Error; err != nil {
		return &pb.UpdateRes{Success: false, Message: "题单不存在"}, nil
	}
	if ps.OwnerID != uid {
		return &pb.UpdateRes{Success: false, Message: "只能修改自己的题单"}, nil
	}
	updates := map[string]interface{}{}
	if t := strings.TrimSpace(req.Title); t != "" {
		if utf8.RuneCountInString(t) > maxProblemsetTitleRunes {
			return &pb.UpdateRes{Success: false, Message: "标题过长"}, nil
		}
		// 系统题单标题固定
		if ps.Kind == model.ProblemsetKindCustom {
			updates["title"] = t
		}
	}
	// 允许清空描述：前端始终传 description 字段
	hasBody := false
	if r, ok := khttp.RequestFromServerContext(ctx); ok {
		hasBody = r.ContentLength > 0
	}
	if req.Description != "" || hasBody {
		desc := strings.TrimSpace(req.Description)
		if utf8.RuneCountInString(desc) > maxProblemsetDescRunes {
			return &pb.UpdateRes{Success: false, Message: "描述过长"}, nil
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
					return &pb.UpdateRes{Success: false, Message: "密码处理失败"}, nil
				}
				updates["password_hash"] = hash
			} else if ps.PasswordHash == "" {
				return &pb.UpdateRes{Success: false, Message: "请设置访问密码"}, nil
			}
		} else {
			if req.ClearPassword || vis != model.ProblemsetVisPassword {
				updates["password_hash"] = ""
			}
		}
	}
	if len(updates) > 0 {
		if err := s.db.WithContext(ctx).Model(&ps).Updates(updates).Error; err != nil {
			return &pb.UpdateRes{Success: false, Message: "更新失败"}, nil
		}
		_ = s.db.WithContext(ctx).First(&ps, ps.ID)
	}
	liked := s.likedMap(ctx, uid, []uint{ps.ID})
	favorited := s.favoritedMap(ctx, uid, []uint{ps.ID})
	return &pb.UpdateRes{Success: true, Message: "ok", Data: s.toBrief(&ps, uid, liked[ps.ID], favorited[ps.ID], true)}, nil
}

func (s *ProblemsetService) Delete(ctx context.Context, req *pb.DeleteReq) (*pb.DeleteRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.DeleteRes{Success: false, Message: "请先登录"}, nil
	}
	id := uint(req.Id)
	if id == 0 {
		return &pb.DeleteRes{Success: false, Message: "参数错误"}, nil
	}
	var ps model.Problemset
	if err := s.db.WithContext(ctx).First(&ps, id).Error; err != nil {
		return &pb.DeleteRes{Success: false, Message: "题单不存在"}, nil
	}
	if ps.OwnerID != uid {
		return &pb.DeleteRes{Success: false, Message: "只能删除自己的题单"}, nil
	}
	if ps.Kind != model.ProblemsetKindCustom {
		return &pb.DeleteRes{Success: false, Message: "系统题单不可删除"}, nil
	}
	_ = s.db.WithContext(ctx).Where("problemset_id = ?", ps.ID).Delete(&model.ProblemsetItem{}).Error
	_ = s.db.WithContext(ctx).Where("problemset_id = ?", ps.ID).Delete(&model.ProblemsetLike{}).Error
	if err := s.db.WithContext(ctx).Delete(&ps).Error; err != nil {
		return &pb.DeleteRes{Success: false, Message: "删除失败"}, nil
	}
	return &pb.DeleteRes{Success: true, Message: "ok"}, nil
}

func (s *ProblemsetService) Unlock(ctx context.Context, req *pb.UnlockReq) (*pb.UnlockRes, error) {
	id := uint(req.Id)
	if id == 0 {
		return &pb.UnlockRes{Success: false, Message: "参数错误"}, nil
	}
	var ps model.Problemset
	if err := s.db.WithContext(ctx).First(&ps, id).Error; err != nil {
		return &pb.UnlockRes{Success: false, Message: "题单不存在"}, nil
	}
	if ps.Visibility != model.ProblemsetVisPassword {
		return &pb.UnlockRes{Success: false, Message: "该题单无需密码"}, nil
	}
	if !checkProblemsetPassword(ps.PasswordHash, strings.TrimSpace(req.Password)) {
		return &pb.UnlockRes{Success: false, Message: "密码错误"}, nil
	}
	token := makeProblemsetUnlockToken(ps.ID)
	return &pb.UnlockRes{
		Success: true, Message: "ok",
		Data: &pb.UnlockData{UnlockToken: token, ExpiresIn: int64(problemsetUnlockTTL.Seconds())},
	}, nil
}

func (s *ProblemsetService) Add(ctx context.Context, req *pb.AddReq) (*pb.AddRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.AddRes{Success: false, Message: "请先登录"}, nil
	}
	// ProblemsetID 可选：0 表示仅向题库入库，不加入题单
	problemsetID := uint(req.ProblemsetId)
	problemID := uint(req.ProblemId)
	// 仅按 problemId 加入题单时必须带 problemsetId；按 url 入库时 problemsetId 可省略
	if problemsetID == 0 && problemID > 0 {
		return &pb.AddRes{Success: false, Message: "请提供题单 id"}, nil
	}
	var ps *model.Problemset
	if problemsetID > 0 {
		var row model.Problemset
		if err := s.db.WithContext(ctx).First(&row, problemsetID).Error; err != nil {
			return &pb.AddRes{Success: false, Message: "题单不存在"}, nil
		}
		if row.OwnerID != uid {
			return &pb.AddRes{Success: false, Message: "只能向自己的题单加题"}, nil
		}
		ps = &row
	}

	var problemIDFinal uint
	fetchTriggered := false
	if problemID > 0 {
		var p model.Problem
		if err := s.db.WithContext(ctx).First(&p, problemID).Error; err != nil {
			return &pb.AddRes{Success: false, Message: "题目不存在"}, nil
		}
		problemIDFinal = p.ID
		if s.uc != nil {
			needFetch := strings.TrimSpace(p.ContentMD) == "" || biz.ContentLooksBroken(p.ContentMD)
			if err := s.uc.ForceEnqueueFetch(p.ID, uid); err != nil {
				log.Warnf("problemset add force fetch id=%d: %v", p.ID, err)
			} else if needFetch {
				fetchTriggered = true
			}
		}
	} else if u := strings.TrimSpace(req.Url); u != "" {
		parsed, err := biz.ParseProblemURL(u)
		if err != nil {
			// 200 + success=false：前端 axios 可拿到 code，引导手动加题
			return &pb.AddRes{Success: false, Message: "无法识别该题目链接", Code: "URL_PARSE_FAILED"}, nil
		}
		if s.uc == nil {
			// 无 usecase：仅查库或建空记录
			var existing model.Problem
			err := s.db.WithContext(ctx).Where("platform = ? AND external_id = ?", parsed.Platform, parsed.ExternalID).First(&existing).Error
			if err == gorm.ErrRecordNotFound {
				existing = model.Problem{
					Platform: parsed.Platform, ExternalID: parsed.ExternalID,
					Title: parsed.Title, URL: parsed.URL, Status: model.ProblemStatusPending,
					Tags: model.StringArray{},
				}
				if err := s.db.WithContext(ctx).Create(&existing).Error; err != nil {
					return &pb.AddRes{Success: false, Message: "入库失败"}, nil
				}
			} else if err != nil {
				return &pb.AddRes{Success: false, Message: "查询题目失败"}, nil
			}
			problemIDFinal = existing.ID
		} else {
			p, err := s.uc.UpsertProblemFromParsedForUser(parsed, uid)
			if err != nil || p == nil {
				return &pb.AddRes{Success: false, Message: "题目处理失败"}, nil
			}
			problemIDFinal = p.ID
			// 空题面或损坏题面都会触发后台最高优先级补爬（Upsert 内 ForceEnqueueFetch）
			fetchTriggered = strings.TrimSpace(p.ContentMD) == "" || biz.ContentLooksBroken(p.ContentMD)
		}
	} else {
		return &pb.AddRes{Success: false, Message: "请提供题目 id 或链接"}, nil
	}

	if ps != nil {
		if err := s.linkProblemToSet(ctx, ps.ID, problemIDFinal); err != nil {
			return &pb.AddRes{Success: false, Message: "加入失败"}, nil
		}
	}
	// 回填识别摘要：前端 5s 内确认弹窗「是否为某平台某题」
	platform, title, externalID := "", "", ""
	if problemIDFinal > 0 {
		var p model.Problem
		if s.db.WithContext(ctx).Select("id", "platform", "title", "external_id").First(&p, problemIDFinal).Error == nil {
			platform = p.Platform
			title = p.Title
			externalID = p.ExternalID
		}
	}
	return &pb.AddRes{
		Success: true, Message: "ok",
		Data: &pb.AddData{
			ProblemId: int64(problemIDFinal), FetchTriggered: fetchTriggered,
			Platform: platform, Title: title, ExternalId: externalID,
		},
	}, nil
}

// AddManual 链接无法识别时：用户手动建题；可选加入题单（无需审核）
// problemsetId 为 0 时仅向题库入库。
func (s *ProblemsetService) AddManual(ctx context.Context, req *pb.AddManualReq) (*pb.AddManualRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.AddManualRes{Success: false, Message: "请先登录"}, nil
	}
	// ProblemsetID 可选：0 表示仅向题库入库
	problemsetID := uint(req.ProblemsetId)
	var ps *model.Problemset
	if problemsetID > 0 {
		var row model.Problemset
		if err := s.db.WithContext(ctx).First(&row, problemsetID).Error; err != nil {
			return &pb.AddManualRes{Success: false, Message: "题单不存在"}, nil
		}
		if row.OwnerID != uid {
			return &pb.AddManualRes{Success: false, Message: "只能向自己的题单加题"}, nil
		}
		ps = &row
	}
	if s.uc == nil {
		return &pb.AddManualRes{Success: false, Message: "服务未就绪"}, nil
	}
	p, err := s.uc.CreateManualProblem(uid, req.Title, req.ContentMd, req.SourceUrl, req.Tags)
	if err != nil || p == nil {
		msg := "创建题目失败"
		if err != nil {
			msg = err.Error()
		}
		return &pb.AddManualRes{Success: false, Message: msg}, nil
	}
	if ps != nil {
		if err := s.linkProblemToSet(ctx, ps.ID, p.ID); err != nil {
			return &pb.AddManualRes{Success: false, Message: "加入题单失败"}, nil
		}
	}
	return &pb.AddManualRes{
		Success: true, Message: "ok",
		Data: &pb.AddManualData{ProblemId: int64(p.ID)},
	}, nil
}

// linkProblemToSet 幂等将题目加入题单
func (s *ProblemsetService) linkProblemToSet(ctx context.Context, setID, problemID uint) error {
	var n int64
	_ = s.db.WithContext(ctx).Model(&model.ProblemsetItem{}).
		Where("problemset_id = ? AND problem_id = ?", setID, problemID).
		Count(&n).Error
	if n > 0 {
		return nil
	}
	var maxSort int
	_ = s.db.WithContext(ctx).Model(&model.ProblemsetItem{}).
		Where("problemset_id = ?", setID).
		Select("COALESCE(MAX(sort_order),0)").Scan(&maxSort).Error
	item := model.ProblemsetItem{
		ProblemsetID: setID,
		ProblemID:    problemID,
		SortOrder:    maxSort + 1,
	}
	if err := s.db.WithContext(ctx).Create(&item).Error; err != nil {
		return err
	}
	_ = s.db.WithContext(ctx).Model(&model.Problemset{}).Where("id = ?", setID).
		UpdateColumn("item_count", gorm.Expr("item_count + 1")).Error
	return nil
}

func (s *ProblemsetService) Remove(ctx context.Context, req *pb.RemoveReq) (*pb.RemoveRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.RemoveRes{Success: false, Message: "请先登录"}, nil
	}
	problemsetID := uint(req.ProblemsetId)
	problemID := uint(req.ProblemId)
	if problemsetID == 0 || problemID == 0 {
		return &pb.RemoveRes{Success: false, Message: "参数错误"}, nil
	}
	var ps model.Problemset
	if err := s.db.WithContext(ctx).First(&ps, problemsetID).Error; err != nil {
		return &pb.RemoveRes{Success: false, Message: "题单不存在"}, nil
	}
	if ps.OwnerID != uid {
		return &pb.RemoveRes{Success: false, Message: "只能修改自己的题单"}, nil
	}
	res := s.db.WithContext(ctx).Where("problemset_id = ? AND problem_id = ?", ps.ID, problemID).
		Delete(&model.ProblemsetItem{})
	if res.Error != nil {
		return &pb.RemoveRes{Success: false, Message: "移除失败"}, nil
	}
	if res.RowsAffected > 0 {
		_ = s.db.WithContext(ctx).Model(&model.Problemset{}).
			Where("id = ? AND item_count > 0", ps.ID).
			UpdateColumn("item_count", gorm.Expr("item_count - 1")).Error
	}
	return &pb.RemoveRes{Success: true, Message: "ok"}, nil
}

// Reorder 拖拽排序：按 ids（题单项 id）顺序重写 sort_order 为 0,1,2…
func (s *ProblemsetService) Reorder(ctx context.Context, req *pb.ReorderReq) (*pb.ReorderRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.ReorderRes{Success: false, Message: "请先登录"}, nil
	}
	problemsetID := uint(req.ProblemsetId)
	if problemsetID == 0 || len(req.Ids) == 0 {
		return &pb.ReorderRes{Success: false, Message: "参数错误"}, nil
	}
	// 去重保序
	seen := make(map[uint]struct{}, len(req.Ids))
	ids := make([]uint, 0, len(req.Ids))
	for _, id := range req.Ids {
		if id == 0 {
			continue
		}
		u := uint(id)
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		ids = append(ids, u)
	}
	if len(ids) == 0 {
		return &pb.ReorderRes{Success: false, Message: "顺序列表不能为空"}, nil
	}
	var ps model.Problemset
	if err := s.db.WithContext(ctx).First(&ps, problemsetID).Error; err != nil {
		return &pb.ReorderRes{Success: false, Message: "题单不存在"}, nil
	}
	if ps.OwnerID != uid {
		return &pb.ReorderRes{Success: false, Message: "只能修改自己的题单"}, nil
	}
	// 校验 ids 均属该题单，且覆盖当前全部题单项（防止半量乱序）
	var existing []model.ProblemsetItem
	if err := s.db.WithContext(ctx).Where("problemset_id = ?", ps.ID).Find(&existing).Error; err != nil {
		return &pb.ReorderRes{Success: false, Message: "读取题单项失败"}, nil
	}
	existSet := make(map[uint]struct{}, len(existing))
	for _, it := range existing {
		existSet[it.ID] = struct{}{}
	}
	if len(ids) != len(existSet) {
		return &pb.ReorderRes{Success: false, Message: "顺序列表与题单项不一致"}, nil
	}
	for _, id := range ids {
		if _, ok := existSet[id]; !ok {
			return &pb.ReorderRes{Success: false, Message: "存在不属于该题单的项"}, nil
		}
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
		return &pb.ReorderRes{Success: false, Message: "排序保存失败"}, nil
	}
	return &pb.ReorderRes{Success: true, Message: "ok"}, nil
}

func (s *ProblemsetService) Like(ctx context.Context, req *pb.LikeReq) (*pb.LikeRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.LikeRes{Success: false, Message: "请先登录"}, nil
	}
	id := uint(req.Id)
	if id == 0 {
		return &pb.LikeRes{Success: false, Message: "参数错误"}, nil
	}
	var ps model.Problemset
	if err := s.db.WithContext(ctx).First(&ps, id).Error; err != nil {
		return &pb.LikeRes{Success: false, Message: "题单不存在"}, nil
	}
	// 仅公有题单可点赞；所有者也可赞自己
	if ps.Visibility != model.ProblemsetVisPublic && ps.OwnerID != uid {
		return &pb.LikeRes{Success: false, Message: "该题单不可点赞"}, nil
	}
	var existing model.ProblemsetLike
	err := s.db.WithContext(ctx).Where("user_id = ? AND problemset_id = ?", uid, ps.ID).First(&existing).Error
	liked := false
	if err == gorm.ErrRecordNotFound {
		if err := s.db.WithContext(ctx).Create(&model.ProblemsetLike{UserID: uid, ProblemsetID: ps.ID}).Error; err != nil {
			return &pb.LikeRes{Success: false, Message: "点赞失败"}, nil
		}
		_ = s.db.WithContext(ctx).Model(&model.Problemset{}).Where("id = ?", ps.ID).
			UpdateColumn("like_count", gorm.Expr("like_count + 1")).Error
		liked = true
	} else if err != nil {
		return &pb.LikeRes{Success: false, Message: "点赞失败"}, nil
	} else {
		_ = s.db.WithContext(ctx).Delete(&existing).Error
		_ = s.db.WithContext(ctx).Model(&model.Problemset{}).Where("id = ? AND like_count > 0", ps.ID).
			UpdateColumn("like_count", gorm.Expr("like_count - 1")).Error
		liked = false
	}
	var likeCount int
	_ = s.db.WithContext(ctx).Model(&model.Problemset{}).Select("like_count").Where("id = ?", ps.ID).Scan(&likeCount).Error
	return &pb.LikeRes{
		Success: true, Message: "ok",
		Data: &pb.LikeData{Liked: liked, LikeCount: int32(likeCount)},
	}, nil
}

// Favorite 切换收藏（与点赞分离；仅公有自定义题单）
func (s *ProblemsetService) Favorite(ctx context.Context, req *pb.FavoriteReq) (*pb.FavoriteRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.FavoriteRes{Success: false, Message: "请先登录"}, nil
	}
	id := uint(req.Id)
	if id == 0 {
		return &pb.FavoriteRes{Success: false, Message: "参数错误"}, nil
	}
	var ps model.Problemset
	if err := s.db.WithContext(ctx).First(&ps, id).Error; err != nil {
		return &pb.FavoriteRes{Success: false, Message: "题单不存在"}, nil
	}
	// 仅公有自定义题单可收藏（广场场景）；系统题单不可
	if !IsPublicProblemset(&ps) {
		return &pb.FavoriteRes{Success: false, Message: "仅公开题单可收藏"}, nil
	}
	var existing model.ProblemsetFavorite
	err := s.db.WithContext(ctx).Where("user_id = ? AND problemset_id = ?", uid, ps.ID).First(&existing).Error
	favorited := false
	if err == gorm.ErrRecordNotFound {
		if err := s.db.WithContext(ctx).Create(&model.ProblemsetFavorite{UserID: uid, ProblemsetID: ps.ID}).Error; err != nil {
			return &pb.FavoriteRes{Success: false, Message: "收藏失败"}, nil
		}
		favorited = true
	} else if err != nil {
		return &pb.FavoriteRes{Success: false, Message: "收藏失败"}, nil
	} else {
		_ = s.db.WithContext(ctx).Delete(&existing).Error
		favorited = false
	}
	return &pb.FavoriteRes{
		Success: true, Message: "ok",
		Data: &pb.FavoriteData{Favorited: favorited},
	}, nil
}

// Favorites 我收藏的题单（排除自己的）
func (s *ProblemsetService) Favorites(ctx context.Context, req *pb.FavoritesReq) (*pb.FavoritesRes, error) {
	uid := auth.GetCurrentUserId(ctx)
	if uid == 0 {
		return &pb.FavoritesRes{Success: false, Message: "请先登录"}, nil
	}
	page := int(req.Page)
	if page <= 0 {
		page = 1
	}
	pageSize := int(req.PageSize)
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 50 {
		pageSize = 50
	}
	// 收藏表 join 题单：他人 + 仍 public custom
	var total int64
	base := s.db.WithContext(ctx).Table("problemset_favorites AS f").
		Joins("INNER JOIN problemsets AS p ON p.id = f.problemset_id").
		Where("f.user_id = ?", uid).
		Where("p.owner_id <> ?", uid).
		Where("p.visibility = ? AND p.kind = ?", model.ProblemsetVisPublic, model.ProblemsetKindCustom)
	_ = base.Count(&total).Error
	var list []model.Problemset
	if err := s.db.WithContext(ctx).Table("problemsets AS p").
		Select("p.*").
		Joins("INNER JOIN problemset_favorites AS f ON f.problemset_id = p.id").
		Where("f.user_id = ?", uid).
		Where("p.owner_id <> ?", uid).
		Where("p.visibility = ? AND p.kind = ?", model.ProblemsetVisPublic, model.ProblemsetKindCustom).
		Order("f.created_at DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&list).Error; err != nil {
		return &pb.FavoritesRes{Success: false, Message: "加载失败"}, nil
	}
	setIDs := idsOfSets(list)
	liked := s.likedMap(ctx, uid, setIDs)
	ownerNames := s.batchOwnerNames(ctx, list)
	items := make([]*pb.ProblemsetInfo, 0, len(list))
	for i := range list {
		b := s.toBrief(&list[i], uid, liked[list[i].ID], true, false)
		b.OwnerName = ownerNames[list[i].OwnerID]
		items = append(items, b)
	}
	return &pb.FavoritesRes{
		Success: true, Message: "ok", Data: items,
		Total: total, Page: int64(page), PageSize: int64(pageSize),
	}, nil
}

// ---------- serializers / helpers ----------

func (s *ProblemsetService) toBrief(ps *model.Problemset, viewerID uint, liked, favorited, withDesc bool) *pb.ProblemsetInfo {
	m := &pb.ProblemsetInfo{
		Id:         int64(ps.ID),
		OwnerId:    int64(ps.OwnerID),
		Title:      ps.Title,
		Kind:       ps.Kind,
		Visibility: ps.Visibility,
		LikeCount:  int32(ps.LikeCount),
		ItemCount:  int32(ps.ItemCount),
		Liked:      liked,
		Favorited:  favorited,
		IsOwner:    viewerID > 0 && viewerID == ps.OwnerID,
		CreatedAt:  ps.CreatedAt.Unix(),
		UpdatedAt:  ps.UpdatedAt.Unix(),
		IsSystem:   ps.Kind == model.ProblemsetKindFavorites || ps.Kind == model.ProblemsetKindTodo,
	}
	if withDesc {
		m.Description = ps.Description
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

func (s *ProblemsetService) likedMap(ctx context.Context, userID uint, setIDs []uint) map[uint]bool {
	out := map[uint]bool{}
	if userID == 0 || len(setIDs) == 0 {
		return out
	}
	var rows []model.ProblemsetLike
	_ = s.db.WithContext(ctx).Where("user_id = ? AND problemset_id IN ?", userID, setIDs).Find(&rows).Error
	for _, r := range rows {
		out[r.ProblemsetID] = true
	}
	return out
}

func (s *ProblemsetService) favoritedMap(ctx context.Context, userID uint, setIDs []uint) map[uint]bool {
	out := map[uint]bool{}
	if userID == 0 || len(setIDs) == 0 {
		return out
	}
	var rows []model.ProblemsetFavorite
	_ = s.db.WithContext(ctx).Where("user_id = ? AND problemset_id IN ?", userID, setIDs).Find(&rows).Error
	for _, r := range rows {
		out[r.ProblemsetID] = true
	}
	return out
}

func (s *ProblemsetService) batchProblemsFull(ctx context.Context, ids []uint) map[uint]*model.Problem {
	out := map[uint]*model.Problem{}
	if len(ids) == 0 {
		return out
	}
	var list []model.Problem
	_ = s.db.WithContext(ctx).Where("id IN ?", ids).Find(&list).Error
	for i := range list {
		p := list[i]
		out[p.ID] = &p
	}
	return out
}

func (s *ProblemsetService) batchOwnerNames(ctx context.Context, list []model.Problemset) map[uint]string {
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
	res, err := client.GetByIds(ctx, &profile.GetByIdsReq{UserIds: ids, OrgId: orgID})
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

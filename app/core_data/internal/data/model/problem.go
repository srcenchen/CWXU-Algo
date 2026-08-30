package model

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"time"
)

const (
	ProblemStatusPending    = "PENDING"  // 待爬取
	ProblemStatusFetching   = "FETCHING" // 爬取中
	ProblemStatusTagging    = "TAGGING"  // 题面已就绪，待/正在 AI 分析
	ProblemStatusCompleted  = "COMPLETED"
	ProblemStatusFailed     = "FAILED"      // 可重试失败（网络/WAF 等）
	ProblemStatusFailedPerm = "FAILED_PERM" // 永久失败/黑名单，不再重试（未找到题面等）
	ProblemStatusSkipped    = "SKIPPED"
)

// StringArray JSON 数组字段
type StringArray []string

func (a StringArray) Value() (driver.Value, error) {
	if a == nil {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(a)
	return b, err
}

func (a *StringArray) Scan(value interface{}) error {
	if value == nil {
		*a = StringArray{}
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		*a = StringArray{}
		return nil
	}
	if len(b) == 0 {
		*a = StringArray{}
		return nil
	}
	return json.Unmarshal(b, a)
}

// SolutionsMeta AI 识别的可用解法
type SolutionMeta struct {
	Name             string `json:"name"`
	TimeComplexity   string `json:"time_complexity"`
	SpaceComplexity  string `json:"space_complexity"`
	BriefExplanation string `json:"brief_explanation"`
}

type SolutionsMeta []SolutionMeta

func (s SolutionsMeta) Value() (driver.Value, error) {
	if s == nil {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(s)
	return b, err
}

func (s *SolutionsMeta) Scan(value interface{}) error {
	if value == nil {
		*s = SolutionsMeta{}
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		*s = SolutionsMeta{}
		return nil
	}
	if len(b) == 0 {
		*s = SolutionsMeta{}
		return nil
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = SolutionsMeta{}
		return nil
	}
	// Older imports stored a single solution object instead of the current
	// array-shaped JSON value. Treat it as a one-item list so one malformed
	// legacy row cannot block problem maintenance recovery or profile rebuilds.
	if trimmed[0] == '{' {
		var item SolutionMeta
		if err := json.Unmarshal(trimmed, &item); err != nil {
			return err
		}
		*s = SolutionsMeta{item}
		return nil
	}
	var parsed SolutionsMeta
	if err := json.Unmarshal(trimmed, &parsed); err != nil {
		return err
	}
	*s = parsed
	return nil
}

// Problem 全局去重题库
type Problem struct {
	ID                       uint          `gorm:"primaryKey"`
	Platform                 string        `gorm:"size:32;not null;uniqueIndex:idx_platform_external"`
	ExternalID               string        `gorm:"size:128;not null;uniqueIndex:idx_platform_external"`
	Title                    string        `gorm:"size:512"`
	URL                      string        `gorm:"size:1024"`
	ContentMD                string        `gorm:"type:text"`
	ContentSource            string        `gorm:"size:16;default:'official'"`
	ContentSourceURL         string        `gorm:"size:1024"`
	ContentSourceProblemID   string        `gorm:"size:64"`
	ContentSourceStatementID string        `gorm:"size:64"`
	ContentLanguage          string        `gorm:"size:16"`
	ContentFetchedAt         *time.Time    `gorm:"index"`
	AnalyzedAt               *time.Time    `gorm:"index"`
	AnalyzedModel            string        `gorm:"size:128"`
	ProblemType              string        `gorm:"size:128"`
	Tags                     StringArray   `gorm:"type:jsonb;default:'[]'"`
	SolutionsMeta            SolutionsMeta `gorm:"type:jsonb;default:'[]'"`
	Difficulty               string        `gorm:"size:32"`
	Status                   string        `gorm:"size:32;index;default:'PENDING'"`
	ErrorMsg                 string        `gorm:"type:text"`
	// FetchAttempts 题面爬取失败次数（仅 ProcessFetch 累计；AI 分析失败不计）
	// 非瞬时错误 >=3 升为 FAILED_PERM
	FetchAttempts int `gorm:"default:0"`
	// FetchFailSince 首次可恢复（405/WAF 等）爬取失败时间；持续超 24h → FAILED_PERM
	FetchFailSince  *time.Time `gorm:"index"`
	LastSubmittedAt *time.Time `gorm:"index"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// 题库人工修改审核状态
const (
	ProblemEditPending  = "pending"
	ProblemEditApproved = "approved"
	ProblemEditRejected = "rejected"
)

// ProblemEditRequest 题面/标签/难度人工修改申请。
// 普通用户 pending 后由站管/资源审核员审核；站管/审核员本人修改会写记录并 auto-approve。
type ProblemEditRequest struct {
	ID                 uint        `gorm:"primaryKey"`
	ProblemID          uint        `gorm:"index;not null"`
	UserID             uint        `gorm:"index;not null"`
	HasTags            bool        `gorm:"default:false"`
	HasContent         bool        `gorm:"default:false"`
	HasDifficulty      bool        `gorm:"default:false"`
	ProposedTags       StringArray `gorm:"type:jsonb;default:'[]'"`
	ProposedContentMD  string      `gorm:"type:text"`
	ProposedTitle      string      `gorm:"size:512"`
	ProposedDifficulty string      `gorm:"size:32"`
	Note               string      `gorm:"type:text"` // 提交说明
	Status             string      `gorm:"size:16;index;default:'pending'"`
	ReviewerID         *uint       `gorm:"index"`
	ReviewNote         string      `gorm:"type:text"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

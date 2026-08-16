package backup

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
)

const pageSize = 2000

// Progress reports job progress (0-100) with a human message.
type Progress func(pct int, msg string)

// DBs holds the two Postgres connections (Core may be nil if unused).
type DBs struct {
	User *gorm.DB
	Core *gorm.DB
}

// ExportOptions controls a full/partial export into dir (uncompressed layout).
type ExportOptions struct {
	DBs      DBs
	Dir      string // work directory; will contain manifest.json, data/, files/
	Scopes   []string
	Progress Progress
}

// Export writes a goalgo-backup-v1 tree into opts.Dir.
func Export(opts ExportOptions) (*Manifest, error) {
	if opts.DBs.User == nil {
		return nil, fmt.Errorf("user 数据库未连接")
	}
	concrete := ExpandedScopes(opts.Scopes)
	if NeedsCoreDB(concrete) && opts.DBs.Core == nil {
		return nil, fmt.Errorf("未配置 core 数据库（CWXU_CORE_DATABASE_SOURCE 或 data.database 中 dbname=algo_user 可自动推导）")
	}

	dataDir := filepath.Join(opts.Dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}

	report := opts.Progress
	if report == nil {
		report = func(int, string) {}
	}

	tables := TablesForScopes(concrete)
	totalSteps := len(tables)
	if HasScope(concrete, ScopeFiles) {
		totalSteps++
	}
	if totalSteps == 0 {
		totalSteps = 1
	}

	counts := map[string]int64{}
	step := 0
	for _, spec := range tables {
		step++
		db := opts.DBs.User
		if spec.DB == "core" {
			db = opts.DBs.Core
		}
		pct := step * 90 / totalSteps
		report(pct, fmt.Sprintf("导出 %s …", spec.Table))
		n, err := exportTable(db, spec.Table, filepath.Join(dataDir, spec.File))
		if err != nil {
			return nil, fmt.Errorf("导出 %s: %w", spec.Table, err)
		}
		counts[spec.Table] = n
	}

	fileCount := 0
	includeFiles := HasScope(concrete, ScopeFiles)
	if includeFiles {
		step++
		report(step*90/totalSteps, "打包上传文件 …")
		filesDir := filepath.Join(opts.Dir, "files")
		n, err := CopyUploadTree(UploadDir(), filesDir)
		if err != nil {
			return nil, fmt.Errorf("导出上传文件: %w", err)
		}
		fileCount = n
	}

	m := &Manifest{
		Version:      FormatVersion,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
		Scopes:       concrete,
		IncludeFiles: includeFiles,
		TableCounts:  counts,
		FileCount:    fileCount,
		AppHint:      "GoAlgo site backup",
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(opts.Dir, "manifest.json"), raw, 0o644); err != nil {
		return nil, err
	}
	report(95, "写入 manifest 完成")
	return m, nil
}

func exportTable(db *gorm.DB, table, outPath string) (int64, error) {
	f, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// 表尚未 AutoMigrate / 历史库缺表时：写出空文件并继续，避免整站导出因一张表挂掉
	if !tableExists(db, table) {
		return 0, nil
	}

	enc := json.NewEncoder(f)
	var total int64
	var lastID int64 = -1

	// Prefer keyset pagination on id; otherwise offset + ORDER BY 实际存在的列。
	// 不可写死 user_id,day：user_ac_problems 等无 day 列。
	hasID := tableHasColumn(db, table, "id")
	orderBy := buildExportOrderBy(func(col string) bool {
		return tableHasColumn(db, table, col)
	})
	for {
		var rows []map[string]interface{}
		q := db.Table(table).Limit(pageSize)
		if hasID {
			if lastID >= 0 {
				q = q.Where("id > ?", lastID)
			}
			q = q.Order("id ASC")
		} else {
			// 无 id 列的表退回 OFFSET 分页：候选排序列（user_id/day/…）随表不同
			// 且非全表唯一，难以做通用 keyset。此路径仅管理员手动备份使用（低频），
			// 大表 OFFSET 深翻页的开销可接受，保留现状并注明。
			q = q.Offset(int(total))
			if orderBy != "" {
				q = q.Order(orderBy)
			}
		}
		if err := q.Find(&rows).Error; err != nil {
			return total, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			normalizeRow(row)
			if err := enc.Encode(row); err != nil {
				return total, err
			}
			total++
			if hasID {
				if id, ok := asInt64(row["id"]); ok {
					lastID = id
				}
			}
		}
		if len(rows) < pageSize {
			break
		}
	}
	return total, nil
}

// exportOrderCandidates 无 id 表的稳定排序候选（按常见复合主键优先级）。
// 仅选用表中真实存在的列，避免 SQLSTATE 42703。
var exportOrderCandidates = []string{
	"user_id", "day", "problem_key", "platform", "submit_id", "first_ac_at",
}

// buildExportOrderBy 根据 hasCol 拼 ORDER BY 子句（不含 id 键集路径）。
func buildExportOrderBy(hasCol func(string) bool) string {
	if hasCol == nil {
		return ""
	}
	var parts []string
	for _, c := range exportOrderCandidates {
		if hasCol(c) {
			parts = append(parts, quoteIdent(c)+" ASC")
		}
	}
	return strings.Join(parts, ", ")
}

func tableExists(db *gorm.DB, table string) bool {
	if db == nil {
		return false
	}
	var n int64
	err := db.Raw(`
		SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = ?
	`, table).Scan(&n).Error
	return err == nil && n > 0
}

func tableHasColumn(db *gorm.DB, table, col string) bool {
	// postgres information_schema
	var n int64
	err := db.Raw(`
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?
	`, table, col).Scan(&n).Error
	return err == nil && n > 0
}

func asInt64(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case int:
		return int64(x), true
	case uint64:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint:
		return int64(x), true
	case float64:
		return int64(x), true
	case []byte:
		var n int64
		if _, err := fmt.Sscan(string(x), &n); err == nil {
			return n, true
		}
	case string:
		var n int64
		if _, err := fmt.Sscan(x, &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// normalizeRow converts driver-specific types to JSON-friendly values.
func normalizeRow(row map[string]interface{}) {
	for k, v := range row {
		switch x := v.(type) {
		case time.Time:
			row[k] = x.UTC().Format(time.RFC3339Nano)
		case []byte:
			// jsonb / text may arrive as []byte
			s := string(x)
			if json.Valid(x) && (len(x) > 0 && (x[0] == '{' || x[0] == '[')) {
				var any interface{}
				if err := json.Unmarshal(x, &any); err == nil {
					row[k] = any
					continue
				}
			}
			row[k] = s
		}
	}
}

// ZipDir packs srcDir into zipPath (parent dirs created).
func ZipDir(srcDir, zipPath string) error {
	return zipWrite(srcDir, zipPath)
}

// UnzipTo extracts zipPath into destDir.
func UnzipTo(zipPath, destDir string) error {
	return zipRead(zipPath, destDir)
}

// WriteJSON is a tiny helper for tests.
func WriteJSON(w io.Writer, v interface{}) error {
	enc := json.NewEncoder(w)
	return enc.Encode(v)
}

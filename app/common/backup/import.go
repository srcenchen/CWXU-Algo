package backup

import (
	"bufio"
	"cwxu-algo/app/common/utils/legacysecret"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ImportOptions restores a backup tree (unzipped) into DBs.
type ImportOptions struct {
	DBs      DBs
	Dir      string // contains manifest.json, data/, optional files/
	Progress Progress
	// LegacyConfigKey is only used to upgrade old enc:v1 site config backups.
	LegacyConfigKey string
}

// Import performs wipe-and-replace for scopes listed in the package manifest.
func Import(opts ImportOptions) (*Manifest, error) {
	if opts.DBs.User == nil {
		return nil, fmt.Errorf("user 数据库未连接")
	}
	report := opts.Progress
	if report == nil {
		report = func(int, string) {}
	}

	m, err := ReadManifest(opts.Dir)
	if err != nil {
		return nil, err
	}
	if m.Version != FormatVersion {
		return nil, fmt.Errorf("不支持的备份版本: %s（需要 %s）", m.Version, FormatVersion)
	}
	concrete := ExpandedScopes(m.Scopes)
	if NeedsCoreDB(concrete) && opts.DBs.Core == nil {
		return nil, fmt.Errorf("未配置 core 数据库，无法导入训练/题库数据")
	}

	tables := TablesForScopes(concrete)
	for _, spec := range tables {
		if spec.File == "site_configs.ndjson" {
			if err := preprocessLegacySiteConfig(filepath.Join(opts.Dir, "data", spec.File), opts.LegacyConfigKey); err != nil {
				return nil, err
			}
		}
	}
	// truncate reverse order to respect FKs within each DB
	report(5, "清空目标表 …")
	if err := truncateTables(opts.DBs, reverseTables(tables)); err != nil {
		return nil, err
	}

	totalSteps := len(tables)
	if m.IncludeFiles && HasScope(concrete, ScopeFiles) {
		totalSteps++
	}
	if totalSteps == 0 {
		totalSteps = 1
	}

	for i, spec := range tables {
		pct := 10 + i*80/totalSteps
		report(pct, fmt.Sprintf("导入 %s …", spec.Table))
		db := opts.DBs.User
		if spec.DB == "core" {
			db = opts.DBs.Core
		}
		path := filepath.Join(opts.Dir, "data", spec.File)
		n, err := importTable(db, spec, path)
		if err != nil {
			return nil, fmt.Errorf("导入 %s: %w（可能已部分恢复，请用备份重新导入）", spec.Table, err)
		}
		if m.TableCounts != nil {
			if expect, ok := m.TableCounts[spec.Table]; ok && expect != n {
				// warn only — still continue
				report(pct, fmt.Sprintf("%s 行数 %d（清单 %d）", spec.Table, n, expect))
			}
		}
		if err := fixSequence(db, spec); err != nil {
			return nil, fmt.Errorf("校正序列 %s: %w", spec.Table, err)
		}
	}

	if m.IncludeFiles && HasScope(concrete, ScopeFiles) {
		report(92, "还原上传文件 …")
		src := filepath.Join(opts.Dir, "files")
		if st, err := os.Stat(src); err == nil && st.IsDir() {
			// replace upload tree contents
			if err := ReplaceUploadTree(src, UploadDir()); err != nil {
				return nil, fmt.Errorf("还原上传文件: %w", err)
			}
		}
	}

	report(98, "导入完成")
	return m, nil
}

var legacySiteSecretColumns = []string{"smtp_password", "agent_secret", "ai_analyze_secret", "upyun_password", "oj_luogu_password", "oj_qoj_password", "payfm_secret"}

func preprocessLegacySiteConfig(filePath, configuredKey string) error {
	raw, err := os.ReadFile(filePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取旧站点配置备份: %w", err)
	}
	lines := strings.Split(string(raw), "\n")
	changed := false
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row map[string]interface{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return fmt.Errorf("预检 site_configs.ndjson: %w", err)
		}
		for _, column := range legacySiteSecretColumns {
			value, _ := row[column].(string)
			if !legacysecret.IsEncrypted(value) {
				continue
			}
			plain, err := legacysecret.DecryptWithKey(value, legacysecret.ResolveKey(configuredKey))
			if err != nil {
				return fmt.Errorf("旧配置备份包含 enc:v1 密文，无法解密 %s；请先在升级环境完成迁移或提供旧配置密钥: %w", column, err)
			}
			row[column], changed = plain, true
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			return err
		}
		lines[i] = string(encoded)
	}
	if !changed {
		return nil
	}
	tmp := filePath + ".migrating"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filePath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ReadManifest loads manifest.json from an unzipped backup directory.
func ReadManifest(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("读取 manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("解析 manifest: %w", err)
	}
	return &m, nil
}

func reverseTables(in []TableSpec) []TableSpec {
	out := make([]TableSpec, len(in))
	for i := range in {
		out[len(in)-1-i] = in[i]
	}
	return out
}

func truncateTables(dbs DBs, tables []TableSpec) error {
	// group by db to run TRUNCATE with multiple tables when possible
	userTables := []string{}
	coreTables := []string{}
	for _, t := range tables {
		if t.DB == "core" {
			coreTables = append(coreTables, t.Table)
		} else {
			userTables = append(userTables, t.Table)
		}
	}
	if len(userTables) > 0 {
		if err := truncateList(dbs.User, userTables); err != nil {
			return err
		}
	}
	if len(coreTables) > 0 {
		if err := truncateList(dbs.Core, coreTables); err != nil {
			return err
		}
	}
	return nil
}

func truncateList(db *gorm.DB, tables []string) error {
	if db == nil || len(tables) == 0 {
		return nil
	}
	// 只截断实际存在的表（与导出侧 tableExists 一致，避免历史库缺表导致整站恢复失败）
	existing := make([]string, 0, len(tables))
	for _, t := range tables {
		if tableExists(db, t) {
			existing = append(existing, t)
		}
	}
	if len(existing) == 0 {
		return nil
	}
	parts := make([]string, len(existing))
	for i, t := range existing {
		parts[i] = quoteIdent(t)
	}
	sql := fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", strings.Join(parts, ", "))
	return db.Exec(sql).Error
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func importTable(db *gorm.DB, spec TableSpec, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	// 目标表不存在：无数据可视为跳过；有数据则明确报错，避免静默丢行
	if !tableExists(db, spec.Table) {
		st, _ := f.Stat()
		if st != nil && st.Size() > 0 {
			return 0, fmt.Errorf("表 %s 不存在，但备份中有数据；请先启动 user 服务完成 AutoMigrate", spec.Table)
		}
		return 0, nil
	}

	sc := bufio.NewScanner(f)
	// large problem content / paste
	buf := make([]byte, 0, 1024*1024)
	sc.Buffer(buf, 32*1024*1024)

	var batch []map[string]interface{}
	var total int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		// CreateInBatches with map requires Table()
		if err := db.Table(spec.Table).Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(batch, 200).Error; err != nil {
			// fallback without on conflict
			if err2 := db.Session(&gorm.Session{SkipHooks: true}).Table(spec.Table).CreateInBatches(batch, 200).Error; err2 != nil {
				return err2
			}
		}
		total += int64(len(batch))
		batch = batch[:0]
		return nil
	}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row map[string]interface{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return total, fmt.Errorf("JSON 行解析失败: %w", err)
		}
		coerceRowTypes(row)
		batch = append(batch, row)
		if len(batch) >= 200 {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return total, err
	}
	if err := flush(); err != nil {
		return total, err
	}
	return total, nil
}

// coerceRowTypes converts JSON numbers for integer-ish keys so Postgres gets int8 not float8.
func coerceRowTypes(row map[string]interface{}) {
	for k, v := range row {
		switch x := v.(type) {
		case float64:
			// whole numbers → int64 (ids, counts, ports…)
			if x == float64(int64(x)) {
				row[k] = int64(x)
			}
		case map[string]interface{}, []interface{}:
			// jsonb: re-encode so driver can store as JSON
			b, err := json.Marshal(x)
			if err == nil {
				row[k] = b
			}
		}
	}
}

func fixSequence(db *gorm.DB, spec TableSpec) error {
	if db == nil || spec.SeqCol == "" {
		return nil
	}
	// setval to MAX(id); if empty table, leave sequence
	sql := fmt.Sprintf(`
		SELECT setval(
			pg_get_serial_sequence('%s', '%s'),
			COALESCE((SELECT MAX(%s) FROM %s), 1),
			(SELECT MAX(%s) IS NOT NULL FROM %s)
		)
	`, spec.Table, spec.SeqCol, quoteIdent(spec.SeqCol), quoteIdent(spec.Table), quoteIdent(spec.SeqCol), quoteIdent(spec.Table))
	// pg_get_serial_sequence needs unquoted table name typically
	sql = fmt.Sprintf(`
		DO $$
		DECLARE
			seq regclass;
			mx bigint;
		BEGIN
			seq := pg_get_serial_sequence('%s', '%s');
			IF seq IS NULL THEN
				RETURN;
			END IF;
			EXECUTE format('SELECT MAX(%%I) FROM %%I', '%s', '%s') INTO mx;
			IF mx IS NULL THEN
				PERFORM setval(seq, 1, false);
			ELSE
				PERFORM setval(seq, mx, true);
			END IF;
		END $$;
	`, spec.Table, spec.SeqCol, spec.SeqCol, spec.Table)
	return db.Exec(sql).Error
}

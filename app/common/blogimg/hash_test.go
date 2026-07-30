package blogimg

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestContentHashStable(t *testing.T) {
	h1 := ContentHash([]byte("hello"))
	h2 := ContentHash([]byte("hello"))
	if h1 == "" || h1 != h2 {
		t.Fatalf("hash unstable: %q %q", h1, h2)
	}
	if ContentHash([]byte("world")) == h1 {
		t.Fatal("different content same hash")
	}
	if ContentHash(nil) != "" {
		t.Fatal("empty should be empty hash")
	}
}

func TestResolveContentHashesBatchChunksAssetLookupKeys(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE blog_image_assets (
		id integer PRIMARY KEY, user_id integer, object_key text, url text, content_hash text, status text
	)`).Error; err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	for i := 0; i < 1201; i++ {
		fmt.Fprintf(&content, "![%d](/blog/44/%064x.webp)\n", i, i+1)
	}
	queries := 0
	maxVars := 0
	if err := db.Callback().Query().Before("gorm:query").Register("test:hash-key-chunks", func(tx *gorm.DB) {
		if tx.Statement.Table == "blog_image_assets" {
			queries++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().After("gorm:query").Register("test:hash-key-chunk-vars", func(tx *gorm.DB) {
		if tx.Statement.Table == "blog_image_assets" && len(tx.Statement.Vars) > maxVars {
			maxVars = len(tx.Statement.Vars)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveContentHashesBatchChecked(db, []ContentHashInput{{ID: 1, UserID: 44, Content: content.String()}}); err != nil {
		t.Fatal(err)
	}
	if queries != 4 {
		t.Fatalf("asset lookup queries=%d, want 4 bounded chunks", queries)
	}
	if maxVars == 0 || maxVars > contentHashAssetKeyBatchSize+1 {
		t.Fatalf("max SQL variables=%d, want at most %d keys plus one user", maxVars, contentHashAssetKeyBatchSize)
	}
}

func TestEncodeDecodeImageHashes(t *testing.T) {
	h := ContentHash([]byte("a"))
	raw := EncodeImageHashes([]string{h, h, "  " + strings.ToUpper(h) + " ", "nope"})
	got := DecodeImageHashes(raw)
	if len(got) != 1 || got[0] != h {
		t.Fatalf("got %v raw=%s", got, raw)
	}
	if len(DecodeImageHashes("")) != 0 || len(DecodeImageHashes("[]")) != 0 {
		t.Fatal("empty")
	}
}

func TestObjectKeyForHashAndExtract(t *testing.T) {
	h := ContentHash([]byte("img"))
	key := ObjectKeyForHash(27, h, ".webp")
	wantPrefix := "/blog/27/" + h + ".webp"
	if key != wantPrefix {
		t.Fatalf("key=%q want %q", key, wantPrefix)
	}
	if got := HashFromObjectKey(key); got != h {
		t.Fatalf("extract=%q want %q", got, h)
	}
	// legacy random name
	if HashFromObjectKey("/blog/27/20260730_1cfd654d4a20de794600d47ad991590d.webp") == "" {
		// 32-hex tail after date_ is valid extract
		t.Fatal("legacy date_hex should extract tail hash")
	}
	if HashFromObjectKey("/blog/27/20260730_short.webp") != "" {
		t.Fatal("short random should not count as content hash")
	}
}

func TestResolveContentHashes(t *testing.T) {
	dsn := "file:hash_resolve?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	type row struct {
		ID          uint `gorm:"primaryKey"`
		CreatedAt   time.Time
		UserID      uint   `gorm:"column:user_id"`
		ObjectKey   string `gorm:"column:object_key"`
		URL         string `gorm:"column:url"`
		ContentHash string `gorm:"column:content_hash"`
		Status      string `gorm:"column:status"`
	}
	if err := db.Table("blog_image_assets").AutoMigrate(&row{}); err != nil {
		t.Fatal(err)
	}
	h := ContentHash([]byte("pic"))
	key := ObjectKeyForHash(9, h, ".png")
	_ = db.Table("blog_image_assets").Create(&row{
		UserID: 9, ObjectKey: key, URL: key, ContentHash: h,
	}).Error
	content := "![x](" + key + ")\n"
	got := ResolveContentHashes(db, 9, content, "")
	if len(got) != 1 || got[0] != h {
		t.Fatalf("got %v", got)
	}
}

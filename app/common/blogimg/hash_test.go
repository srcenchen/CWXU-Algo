package blogimg

import (
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

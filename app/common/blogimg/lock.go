package blogimg

import (
	"fmt"
	"hash/fnv"
	"sync"

	"gorm.io/gorm"
)

var userImageReferenceLocks [256]sync.Mutex
var allImageReferencesLock sync.RWMutex

func localUserImageReferenceLock(userID uint) *sync.Mutex {
	return &userImageReferenceLocks[userID%uint(len(userImageReferenceLocks))]
}

func userImageReferenceAdvisoryKey(userID uint) int64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "goalgo:blog-image-references:%d", userID)
	return int64(h.Sum64())
}

func allImageReferencesAdvisoryKey() int64 {
	h := fnv.New64a()
	_, _ = fmt.Fprint(h, "goalgo:all-blog-image-references")
	return int64(h.Sum64())
}

// WithUserImageReferenceTx serializes GC and every write that may change a
// user's image references. PostgreSQL uses a transaction-scoped advisory lock
// for cross-instance coordination; the local lock also makes SQLite tests and
// same-process callers deterministic.
func WithUserImageReferenceTx(db *gorm.DB, userID uint, fn func(tx *gorm.DB) error) error {
	if db == nil || userID == 0 || fn == nil {
		return fmt.Errorf("invalid user image reference transaction")
	}
	allImageReferencesLock.RLock()
	defer allImageReferencesLock.RUnlock()
	lock := localUserImageReferenceLock(userID)
	lock.Lock()
	defer lock.Unlock()
	return db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec(`SELECT pg_advisory_xact_lock_shared(?)`, allImageReferencesAdvisoryKey()).Error; err != nil {
				return fmt.Errorf("acquire shared image reference advisory lock: %w", err)
			}
			if err := tx.Exec(`SELECT pg_advisory_xact_lock(?)`, userImageReferenceAdvisoryKey(userID)).Error; err != nil {
				return fmt.Errorf("acquire user image reference advisory lock: %w", err)
			}
		}
		return fn(tx)
	})
}

// WithAdminImageReferenceTx blocks every image-reference writer while an
// administrator performs a fresh global reference check and one deletion.
func WithAdminImageReferenceTx(db *gorm.DB, fn func(tx *gorm.DB) error) error {
	if db == nil || fn == nil {
		return fmt.Errorf("invalid admin image reference transaction")
	}
	allImageReferencesLock.Lock()
	defer allImageReferencesLock.Unlock()
	return db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec(`SELECT pg_advisory_xact_lock(?)`, allImageReferencesAdvisoryKey()).Error; err != nil {
				return fmt.Errorf("acquire admin image reference advisory lock: %w", err)
			}
		}
		return fn(tx)
	})
}

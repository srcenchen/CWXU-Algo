package blogimg

import (
	"fmt"
	"hash/fnv"
	"sync"

	"gorm.io/gorm"
)

var userImageReferenceLocks [256]sync.Mutex

func localUserImageReferenceLock(userID uint) *sync.Mutex {
	return &userImageReferenceLocks[userID%uint(len(userImageReferenceLocks))]
}

func userImageReferenceAdvisoryKey(userID uint) int64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "goalgo:blog-image-references:%d", userID)
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
	lock := localUserImageReferenceLock(userID)
	lock.Lock()
	defer lock.Unlock()
	return db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec(`SELECT pg_advisory_xact_lock(?)`, userImageReferenceAdvisoryKey(userID)).Error; err != nil {
				return fmt.Errorf("acquire user image reference advisory lock: %w", err)
			}
		}
		return fn(tx)
	})
}

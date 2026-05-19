package translator

import (
	"crypto/sha256"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var bucketTranslations = []byte("translations")

type Cache struct {
	db *bolt.DB
}

func NewCache(path string) (*Cache, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("opening translation cache: %w", err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketTranslations)
		return err
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("creating cache bucket: %w", err)
	}

	return &Cache{db: db}, nil
}

func cacheKey(text, sourceLang, targetLang string) []byte {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s", sourceLang, targetLang, text)))
	return h[:]
}

func (c *Cache) Get(text, sourceLang, targetLang string) (string, bool) {
	var result string
	var found bool

	c.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketTranslations).Get(cacheKey(text, sourceLang, targetLang))
		if data != nil {
			result = string(data)
			found = true
		}
		return nil
	})

	return result, found
}

func (c *Cache) Put(text, sourceLang, targetLang, translated string) {
	c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketTranslations).Put(
			cacheKey(text, sourceLang, targetLang),
			[]byte(translated),
		)
	})
}

func (c *Cache) Close() error {
	return c.db.Close()
}

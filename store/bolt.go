package store

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketMsgMap    = []byte("msg_map")
	bucketMsgMapRev = []byte("msg_map_rev")
)

type MessageMapping struct {
	SrcChannelID int64 `json:"src_channel_id"`
	SrcMsgID     int   `json:"src_msg_id"`
	DstChannelID int64 `json:"dst_channel_id"`
	DstMsgID     int   `json:"dst_msg_id"`
	TopMsgID     int   `json:"top_msg_id,omitempty"` // forum topic ID in source channel
	Timestamp    int64 `json:"timestamp"`
}

type Store struct {
	db *bolt.DB
}

func NewStore(path string) (*Store, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("opening bolt db: %w", err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{bucketMsgMap, bucketMsgMapRev} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("creating buckets: %w", err)
	}

	return &Store{db: db}, nil
}

func msgKey(channelID int64, msgID int) []byte {
	return []byte(fmt.Sprintf("%d:%d", channelID, msgID))
}

func (s *Store) SaveMapping(m MessageMapping) error {
	m.Timestamp = time.Now().Unix()

	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshaling mapping: %w", err)
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		srcKey := msgKey(m.SrcChannelID, m.SrcMsgID)
		dstKey := msgKey(m.DstChannelID, m.DstMsgID)

		if err := tx.Bucket(bucketMsgMap).Put(srcKey, data); err != nil {
			return err
		}
		return tx.Bucket(bucketMsgMapRev).Put(dstKey, data)
	})
}

func (s *Store) LookupBySource(channelID int64, msgID int) (*MessageMapping, error) {
	return s.lookup(bucketMsgMap, channelID, msgID)
}

func (s *Store) LookupByDestination(channelID int64, msgID int) (*MessageMapping, error) {
	return s.lookup(bucketMsgMapRev, channelID, msgID)
}

func (s *Store) lookup(bucket []byte, channelID int64, msgID int) (*MessageMapping, error) {
	var m MessageMapping
	var found bool

	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucket).Get(msgKey(channelID, msgID))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &m)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &m, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

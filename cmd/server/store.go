package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
	"multifrp/internal/control"
)

type device struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Revoked   bool   `json:"revoked"`
	TokenHash string `json:"tokenHash,omitempty"`
	InviteID  string `json:"inviteID,omitempty"`
}

type invitation struct {
	Code      string    `json:"code,omitempty"`
	Remark    string    `json:"remark,omitempty"`
	DeviceID  string    `json:"deviceID,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	Expires   time.Time `json:"expires"`
}

func (i invitation) available(now time.Time) bool {
	return i.DeviceID == "" && i.Expires.After(now)
}

type invitations map[string]invitation

func (i *invitations) UnmarshalJSON(data []byte) error {
	var records map[string]json.RawMessage
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}
	result := make(invitations, len(records))
	for id, raw := range records {
		var entry invitation
		target := any(&entry)
		if len(raw) > 0 && raw[0] == '"' {
			// Older databases store only the expiry date under the code's hash.
			target = &entry.Expires
		}
		if err := json.Unmarshal(raw, target); err != nil {
			return err
		}
		result[id] = entry
	}
	*i = result
	return nil
}

type state struct {
	Domain  string                    `json:"domain"`
	Devices map[string]device         `json:"devices"`
	Invites invitations               `json:"invites"`
	Tunnels map[string]control.Tunnel `json:"tunnels"`
}

type traffic struct {
	In  uint64 `json:"in"`
	Out uint64 `json:"out"`
}

type trafficPoint struct {
	Time int64 `json:"time"`
	traffic
}

var stateBucket = []byte("state")
var trafficBucket = []byte("traffic")
var stateKey = []byte("current")

func openStore(path, domain string) (*bolt.DB, state, error) {
	var data state
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, data, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, data, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(stateBucket)
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(trafficBucket); err != nil {
			return err
		}
		if raw := bucket.Get(stateKey); raw != nil {
			return json.Unmarshal(raw, &data)
		}
		data = state{domain, map[string]device{}, invitations{}, map[string]control.Tunnel{}}
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		return bucket.Put(stateKey, raw)
	})
	if err != nil {
		_ = db.Close()
	}
	return db, data, err
}

func saveState(db *bolt.DB, candidate state) error {
	raw, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(stateBucket).Put(stateKey, raw)
	})
}

func stamp(value int64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, uint64(value))
	return key
}

func readTraffic(raw []byte) traffic {
	if len(raw) != 16 {
		return traffic{}
	}
	return traffic{binary.BigEndian.Uint64(raw[:8]), binary.BigEndian.Uint64(raw[8:])}
}

func addTraffic(db *bolt.DB, at time.Time, samples map[string]traffic, prune bool) error {
	return db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(trafficBucket)
		key := stamp(at.Unix())
		for id, delta := range samples {
			bucket, err := root.CreateBucketIfNotExists([]byte(id))
			if err != nil {
				return err
			}
			old := readTraffic(bucket.Get(key))
			raw := append(stamp(int64(old.In+delta.In)), stamp(int64(old.Out+delta.Out))...)
			if err := bucket.Put(key, raw); err != nil {
				return err
			}
		}
		if !prune {
			return nil
		}
		cutoff := at.AddDate(0, 0, -8).Unix()
		return root.ForEach(func(id, _ []byte) error {
			cursor := root.Bucket(id).Cursor()
			for key, _ := cursor.First(); key != nil && int64(binary.BigEndian.Uint64(key)) < cutoff; key, _ = cursor.Next() {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func queryTraffic(bucket *bolt.Bucket, since time.Time, step time.Duration) (traffic, []trafficPoint) {
	var total traffic
	points := []trafficPoint{}
	if bucket == nil {
		return total, points
	}
	cursor := bucket.Cursor()
	for key, raw := cursor.Seek(stamp(since.Unix())); key != nil; key, raw = cursor.Next() {
		value := readTraffic(raw)
		total.In += value.In
		total.Out += value.Out
		if step == 0 {
			continue
		}
		at := time.Unix(int64(binary.BigEndian.Uint64(key)), 0).Truncate(step).Unix()
		if len(points) == 0 || points[len(points)-1].Time != at {
			points = append(points, trafficPoint{Time: at})
		}
		points[len(points)-1].In += value.In
		points[len(points)-1].Out += value.Out
	}
	return total, points
}

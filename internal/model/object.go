package model

import "time"

// An Object represents the blob stored on the filesystem.
type Object struct {
	Base `json:",inline" storm:"inline"`

	ContainerID string `json:"container_id" storm:"index"`
	ManifestID  string `json:"manifest_id"  storm:"index"`

	// CKey scopes Key to its container so that a lookup by (container, key)
	// reads an index rather than scanning every object in the database.
	// Save keeps it in step with the two fields it derives from.
	CKey string `json:"ckey" storm:"index"`

	Key         string    `json:"key"          storm:"index"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	Checksum    string    `json:"checksum"`
	TTL         time.Time `json:"ttl"          storm:"index"`
}

// ScopedKey names an object within its container, the form CKey holds.
func ScopedKey(containerID, key string) string {
	return containerID + "/" + key
}

// SyncKeys derives the index keys from the fields they scope.
func (o *Object) SyncKeys() {
	o.CKey = ScopedKey(o.ContainerID, o.Key)
}

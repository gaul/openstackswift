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

	Key                string    `json:"key"          storm:"index"`
	Size               int64     `json:"size"`
	ContentType        string    `json:"content_type"`
	ContentDisposition string    `json:"content_disposition"`
	ContentEncoding    string    `json:"content_encoding"`
	CacheControl       string    `json:"cache_control"`
	Expires            string    `json:"expires"`
	Checksum           string    `json:"checksum"`
	TTL                time.Time `json:"ttl"          storm:"index"`

	// Static marks this object as a Static Large Object (SLO) manifest.  Such
	// an object has no backing file; its content is the concatenation of the
	// Segments below, in order.  Unlike a Dynamic Large Object (which gathers
	// its segments by a shared prefix), an SLO stores an explicit segment list.
	Static   bool      `json:"static"`
	Segments []Segment `json:"segments"`
}

// A Segment references one object that backs a Static Large Object manifest.
type Segment struct {
	Container string `json:"container"`
	Object    string `json:"object"`
	Size      int64  `json:"size"`
	Etag      string `json:"etag"`
}

// ScopedKey names an object within its container, the form CKey holds.
func ScopedKey(containerID, key string) string {
	return containerID + "/" + key
}

// SyncKeys derives the index keys from the fields they scope.
func (o *Object) SyncKeys() {
	o.CKey = ScopedKey(o.ContainerID, o.Key)
}

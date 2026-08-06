package model

// A Manifest represents aggregates an blob across several Objects used by chunked upload.
type Manifest struct {
	Base `json:",inline" storm:"inline"`

	ContainerID string `json:"container_id" storm:"index"`

	// CKey scopes Key to its container; see Object.CKey.
	CKey string `json:"ckey" storm:"index"`

	// URI         string `json:"uri"`
	Key         string `json:"key"          storm:"index"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	// FilePath    string `json:"file_path"`
	Checksum string `json:"checksum"`
}

// SyncKeys derives the index keys from the fields they scope.
func (m *Manifest) SyncKeys() {
	m.CKey = ScopedKey(m.ContainerID, m.Key)
}

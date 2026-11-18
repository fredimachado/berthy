package api

// KeyRotation is what `key rotate` answers: the state key that was replaced and the one that replaces it. The IDs name keys
// and never reveal them. With a dry run nothing changed: NewKeyID is empty and Values is how many sealed values a rotation
// would seal again.
type KeyRotation struct {
	OldKeyID string `json:"old_key_id"`
	NewKeyID string `json:"new_key_id,omitempty"`
	Values   int    `json:"values"`
	// KeyFile is where the key is on the server, which the person reads it from to keep a copy.
	KeyFile string `json:"key_file"`
	DryRun  bool   `json:"dry_run,omitempty"`
}

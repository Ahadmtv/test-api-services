package platform

import "strings"

// StaticImagePrefix is the public HTTP prefix for server-managed images. Files
// are named with server-generated UUIDs; no caller-controlled path segment is
// ever used to build a stored filename.
const StaticImagePrefix = "/static/img"

// PublicImageURL converts a validated storage key into the public URL returned
// by the API. It never exposes the configured filesystem directory.
func PublicImageURL(storageKey string) string {
	storageKey = strings.TrimSpace(storageKey)
	if storageKey == "" {
		return ""
	}
	return StaticImagePrefix + "/" + storageKey
}

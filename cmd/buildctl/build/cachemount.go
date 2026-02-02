package build

import (
	"strings"

	"github.com/moby/buildkit/client"
	"github.com/pkg/errors"
	"github.com/tonistiigi/go-csvvalue"
)

// ParseCacheMountEntry parses --cache-mount-export and --cache-mount-import flags
// Format: id=<id>,type=<type>,ref=<ref>[,<opt>=<optval>]
// Example: id=gocache,type=registry,ref=example.com/cache:go
func ParseCacheMountEntry(entries []string) ([]client.CacheMountEntry, error) {
	var result []client.CacheMountEntry
	for _, entry := range entries {
		e, err := parseCacheMountEntryCSV(entry)
		if err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, nil
}

func parseCacheMountEntryCSV(s string) (client.CacheMountEntry, error) {
	entry := client.CacheMountEntry{
		Attrs: map[string]string{},
	}
	fields, err := csvvalue.Fields(s, nil)
	if err != nil {
		return entry, err
	}
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return entry, errors.Errorf("invalid value %s", field)
		}
		key = strings.ToLower(key)
		switch key {
		case "id":
			entry.ID = value
		case "type":
			entry.Type = value
		default:
			entry.Attrs[key] = value
		}
	}
	if entry.ID == "" {
		return entry, errors.New("cache mount entry requires id=<id>")
	}
	if entry.Type == "" {
		return entry, errors.New("cache mount entry requires type=<type>")
	}
	return entry, nil
}

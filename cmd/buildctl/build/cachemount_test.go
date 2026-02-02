package build

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseCacheMountEntry(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		wantLen int
		wantErr bool
	}{
		{
			name:    "empty",
			entries: []string{},
			wantLen: 0,
			wantErr: false,
		},
		{
			name:    "single registry entry",
			entries: []string{"id=gocache,type=registry,ref=example.com/cache:go"},
			wantLen: 1,
			wantErr: false,
		},
		{
			name:    "multiple entries",
			entries: []string{"id=gocache,type=registry,ref=example.com/cache:go", "id=npm,type=registry,ref=example.com/cache:npm"},
			wantLen: 2,
			wantErr: false,
		},
		{
			name:    "missing id",
			entries: []string{"type=registry,ref=example.com/cache:go"},
			wantErr: true,
		},
		{
			name:    "missing type",
			entries: []string{"id=gocache,ref=example.com/cache:go"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseCacheMountEntry(tt.entries)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, result, tt.wantLen)
		})
	}
}

func TestParseCacheMountEntryCSV(t *testing.T) {
	t.Run("valid entry", func(t *testing.T) {
		entry, err := parseCacheMountEntryCSV("id=gocache,type=registry,ref=example.com/cache:go")
		require.NoError(t, err)
		require.Equal(t, "gocache", entry.ID)
		require.Equal(t, "registry", entry.Type)
		require.Equal(t, "example.com/cache:go", entry.Attrs["ref"])
	})

	t.Run("entry with extra attrs", func(t *testing.T) {
		entry, err := parseCacheMountEntryCSV("id=npm,type=registry,ref=example.com/cache:npm,insecure=true")
		require.NoError(t, err)
		require.Equal(t, "npm", entry.ID)
		require.Equal(t, "registry", entry.Type)
		require.Equal(t, "example.com/cache:npm", entry.Attrs["ref"])
		require.Equal(t, "true", entry.Attrs["insecure"])
	})

	t.Run("local type", func(t *testing.T) {
		entry, err := parseCacheMountEntryCSV("id=gocache,type=local,dest=/tmp/cache")
		require.NoError(t, err)
		require.Equal(t, "gocache", entry.ID)
		require.Equal(t, "local", entry.Type)
		require.Equal(t, "/tmp/cache", entry.Attrs["dest"])
	})
}

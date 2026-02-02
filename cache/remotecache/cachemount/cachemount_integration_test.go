package cachemount

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

// TestConfigMarshaling tests that Config can be marshaled and unmarshaled correctly
func TestConfigMarshaling(t *testing.T) {
	createdTime := time.Now().UTC().Truncate(time.Second)
	original := Config{
		ID:          "test-cache",
		Created:     createdTime.Format(time.RFC3339),
		Sharing:     "shared",
		ContentHash: digest.FromString("test").String(),
		Size:        1024,
		FileCount:   10,
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var unmarshaled Config
	err = json.Unmarshal(data, &unmarshaled)
	require.NoError(t, err)

	require.Equal(t, original.ID, unmarshaled.ID)
	require.Equal(t, original.Created, unmarshaled.Created)
	require.Equal(t, original.Sharing, unmarshaled.Sharing)
	require.Equal(t, original.ContentHash, unmarshaled.ContentHash)
	require.Equal(t, original.Size, unmarshaled.Size)
	require.Equal(t, original.FileCount, unmarshaled.FileCount)
}

// TestManifestFormat tests that the manifest format is correct
func TestManifestFormat(t *testing.T) {
	// Create a mock config
	config := Config{
		ID:        "test",
		Created:   time.Now().UTC(),
		Size:      1024,
		FileCount: 5,
	}
	configData, err := json.Marshal(config)
	require.NoError(t, err)
	configDigest := digest.FromBytes(configData)

	// Create a mock layer
	var layerBuf bytes.Buffer
	gzw := gzip.NewWriter(&layerBuf)
	tw := tar.NewWriter(gzw)
	
	// Write a test file
	content := []byte("test content")
	err = tw.WriteHeader(&tar.Header{
		Name: "test.txt",
		Mode: 0644,
		Size: int64(len(content)),
	})
	require.NoError(t, err)
	_, err = tw.Write(content)
	require.NoError(t, err)
	
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	
	layerDigest := digest.FromBytes(layerBuf.Bytes())

	// Create manifest
	manifest := ocispecs.Manifest{
		MediaType: ocispecs.MediaTypeImageManifest,
		Config: ocispecs.Descriptor{
			MediaType: MediaTypeCacheMountConfig,
			Digest:    configDigest,
			Size:      int64(len(configData)),
		},
		Layers: []ocispecs.Descriptor{
			{
				MediaType: ocispecs.MediaTypeImageLayerGzip,
				Digest:    layerDigest,
				Size:      int64(layerBuf.Len()),
				Annotations: map[string]string{
					AnnotationCacheMountID: "test",
				},
			},
		},
		Annotations: map[string]string{
			AnnotationCacheMountID:      "test",
			AnnotationCacheMountVersion: CurrentVersion,
		},
	}

	manifestData, err := json.Marshal(manifest)
	require.NoError(t, err)

	// Verify manifest can be unmarshaled
	var unmarshaled ocispecs.Manifest
	err = json.Unmarshal(manifestData, &unmarshaled)
	require.NoError(t, err)

	require.Equal(t, MediaTypeCacheMountConfig, unmarshaled.Config.MediaType)
	require.Len(t, unmarshaled.Layers, 1)
	require.Equal(t, "test", unmarshaled.Annotations[AnnotationCacheMountID])
	require.Equal(t, CurrentVersion, unmarshaled.Annotations[AnnotationCacheMountVersion])
}

// TestLayerExtraction tests that tar layers can be read correctly
func TestLayerExtraction(t *testing.T) {
	// Create a test tar.gz
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	files := map[string][]byte{
		"dir/":        nil,
		"dir/file1":   []byte("content1"),
		"dir/file2":   []byte("content2"),
		"readme.txt":  []byte("hello world"),
	}

	for name, content := range files {
		if content == nil {
			// Directory
			err := tw.WriteHeader(&tar.Header{
				Name:     name,
				Mode:     0755,
				Typeflag: tar.TypeDir,
			})
			require.NoError(t, err)
		} else {
			// File
			err := tw.WriteHeader(&tar.Header{
				Name: name,
				Mode: 0644,
				Size: int64(len(content)),
			})
			require.NoError(t, err)
			_, err = tw.Write(content)
			require.NoError(t, err)
		}
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())

	// Read it back
	gzr, err := gzip.NewReader(&buf)
	require.NoError(t, err)
	tr := tar.NewReader(gzr)

	var foundFiles []string
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		foundFiles = append(foundFiles, header.Name)

		if header.Typeflag == tar.TypeReg {
			content, err := io.ReadAll(tr)
			require.NoError(t, err)
			require.Equal(t, files[header.Name], content)
		}
	}

	require.Len(t, foundFiles, len(files))
}

// TestManagerPendingOperations tests the manager's pending operations tracking
func TestManagerPendingOperations(t *testing.T) {
	// Create manager without actual dependencies (nil is ok for this test)
	m := NewManager(nil, nil, nil)

	// Add imports
	m.AddImport(NewImportEntry("gocache", "registry", map[string]string{"ref": "example.com/cache:go"}))
	m.AddImport(NewImportEntry("npm", "registry", map[string]string{"ref": "example.com/cache:npm"}))

	require.True(t, m.HasImport("gocache"))
	require.True(t, m.HasImport("npm"))
	require.False(t, m.HasImport("other"))

	// Add exports
	m.AddExport(NewExportEntry("gocache", "registry", map[string]string{"ref": "example.com/cache:go"}))

	require.True(t, m.HasExport("gocache"))
	require.False(t, m.HasExport("npm"))

	// Clear pending
	m.ClearPending()

	require.False(t, m.HasImport("gocache"))
	require.False(t, m.HasExport("gocache"))
}

// TestIsSubPath tests the path safety check for tar extraction
func TestIsSubPath(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		target string
		want   bool
	}{
		// Valid paths that should be allowed
		{
			name:   "regular file",
			base:   "/tmp/cache",
			target: "/tmp/cache/file.txt",
			want:   true,
		},
		{
			name:   "dotfile",
			base:   "/tmp/cache",
			target: "/tmp/cache/.npmrc",
			want:   true,
		},
		{
			name:   "hidden directory",
			base:   "/tmp/cache",
			target: "/tmp/cache/.cache/files",
			want:   true,
		},
		{
			name:   "gitignore",
			base:   "/tmp/cache",
			target: "/tmp/cache/.gitignore",
			want:   true,
		},
		{
			name:   "nested dotfile",
			base:   "/tmp/cache",
			target: "/tmp/cache/subdir/.env",
			want:   true,
		},
		{
			name:   "dot in filename",
			base:   "/tmp/cache",
			target: "/tmp/cache/file.tar.gz",
			want:   true,
		},

		// Invalid paths that should be blocked (directory traversal)
		{
			name:   "parent directory escape",
			base:   "/tmp/cache",
			target: "/tmp/cache/../secret",
			want:   false,
		},
		{
			name:   "double dot directory",
			base:   "/tmp/cache",
			target: "/tmp/other",
			want:   false,
		},
		{
			name:   "absolute path outside base",
			base:   "/tmp/cache",
			target: "/etc/passwd",
			want:   false,
		},
		{
			name:   "exact parent",
			base:   "/tmp/cache",
			target: "/tmp",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSubPath(tt.base, tt.target)
			require.Equal(t, tt.want, got, "isSubPath(%q, %q) = %v, want %v", tt.base, tt.target, got, tt.want)
		})
	}
}

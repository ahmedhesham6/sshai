package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/ahmedhesham6/sshai/libs/capsule"
)

const (
	maxLayerUncompressedSize = 64 << 20
	maxLayerIndexSize        = 4 << 20
)

type layerIndexEntry struct {
	Digest string `json:"digest"`
	Mode   uint32 `json:"mode"`
}

func parseLayerIndex(layerBytes []byte) ([]capsule.FileIndexEntry, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(layerBytes))
	if err != nil {
		return nil, fmt.Errorf("open gzip layer: %w", err)
	}
	defer gzipReader.Close()
	limitedGzip := &io.LimitedReader{R: gzipReader, N: maxLayerUncompressedSize + 1}
	tarReader := tar.NewReader(limitedGzip)
	var indexJSON []byte
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if limitedGzip.N == 0 {
				return nil, fmt.Errorf("layer decompressed data exceeds maximum uncompressed size %d bytes", maxLayerUncompressedSize)
			}
			return nil, fmt.Errorf("read tar layer: %w", err)
		}
		if header.Name == capsule.IndexPath {
			if indexJSON != nil {
				return nil, errors.New("layer contains duplicate index.json entries")
			}
			indexJSON, err = io.ReadAll(io.LimitReader(tarReader, maxLayerIndexSize+1))
			if err != nil {
				if limitedGzip.N == 0 {
					return nil, fmt.Errorf("layer decompressed data exceeds maximum uncompressed size %d bytes", maxLayerUncompressedSize)
				}
				return nil, fmt.Errorf("read layer index: %w", err)
			}
			if int64(len(indexJSON)) > maxLayerIndexSize {
				return nil, fmt.Errorf("layer index exceeds maximum uncompressed size %d bytes", maxLayerIndexSize)
			}
			continue
		}
		if _, err := io.Copy(io.Discard, tarReader); err != nil {
			if limitedGzip.N == 0 {
				return nil, fmt.Errorf("layer decompressed data exceeds maximum uncompressed size %d bytes", maxLayerUncompressedSize)
			}
			return nil, fmt.Errorf("read tar entry %q: %w", header.Name, err)
		}
	}
	if limitedGzip.N == 0 {
		return nil, fmt.Errorf("layer decompressed data exceeds maximum uncompressed size %d bytes", maxLayerUncompressedSize)
	}
	if indexJSON == nil {
		return nil, errors.New("layer is missing index.json")
	}

	var encoded map[string]layerIndexEntry
	if err := json.Unmarshal(indexJSON, &encoded); err != nil {
		return nil, fmt.Errorf("decode index.json: %w", err)
	}
	entries := make([]capsule.FileIndexEntry, 0, len(encoded))
	paths := make([]string, 0, len(encoded))
	for path := range encoded {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		entry := encoded[path]
		entries = append(entries, capsule.FileIndexEntry{Path: path, Digest: entry.Digest, Mode: entry.Mode})
	}
	canonical, err := (capsule.Layer{Index: entries}).CanonicalIndexJSON()
	if err != nil {
		return nil, fmt.Errorf("validate index.json: %w", err)
	}
	if string(canonical) != string(indexJSON) {
		return nil, errors.New("index.json is not canonical")
	}
	return entries, nil
}

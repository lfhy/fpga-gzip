package firmware

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Manifest travels alongside an immutable bitstream. JSON here is an artifact
// description, not filesystem metadata. All images share ABI/format IDs; each
// image declares its actual algorithm, available DMA banks and build ID.
type Manifest struct {
	Schema     uint32 `json:"schema"`
	Name       string `json:"name"`
	Part       string `json:"part"`
	Algorithm  string `json:"algorithm"`
	ABI        uint32 `json:"abi"`
	Format     uint32 `json:"format"`
	Cores      uint32 `json:"cores"`
	Build      uint64 `json:"build"`
	Legacy     bool   `json:"legacy,omitempty"`
	Bitstream  string `json:"bitstream"`
	SHA256     string `json:"sha256"`
	Validation string `json:"validation"`
	Source     string `json:"source,omitempty"`
}

func LoadManifest(path string) (Manifest, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, "", err
	}
	defer f.Close()
	var m Manifest
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, "", err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return m, "", errors.New("manifest has trailing data")
	}
	if err := m.Validate(); err != nil {
		return m, "", err
	}
	bit := filepath.Join(filepath.Dir(path), m.Bitstream)
	return m, bit, nil
}

func (m Manifest) Validate() error {
	a, err := ParseAlgorithm(m.Algorithm)
	if err != nil {
		return err
	}
	if m.Schema != 1 || m.Name == "" || m.Part != "xcku5p-ffvb676-2-e" || m.Format != FormatV1 || m.Cores < 1 || m.Cores > 4 {
		return errors.New("invalid KU5P firmware manifest")
	}
	if m.Legacy {
		if a != Gzip || m.ABI != 0 || m.Build != 0 {
			return errors.New("legacy manifest must describe the known unversioned gzip image")
		}
	} else if m.ABI>>16 != ABI>>16 || m.Build == 0 {
		return errors.New("manifest requires ABI major 1 and a nonzero build ID")
	}
	if m.Bitstream == "" || filepath.IsAbs(m.Bitstream) || filepath.Clean(m.Bitstream) != filepath.Base(m.Bitstream) || filepath.Ext(m.Bitstream) != ".bit" {
		return errors.New("manifest bitstream must be an adjacent .bit file")
	}
	checksum, err := hex.DecodeString(m.SHA256)
	if err != nil || len(checksum) != sha256.Size {
		return errors.New("manifest requires a SHA256 checksum")
	}
	// Synthesis or C simulation alone must never be advertised as a loadable,
	// hardware-validated artifact. Timing-closed candidates can be programmed
	// for their first board validation, and are identified as candidates.
	if m.Validation != "timing-closed" && m.Validation != "hardware-verified" {
		return errors.New("manifest is not a timing-closed bitstream")
	}
	return nil
}

func (m Manifest) Check(info Info) error {
	if err := info.Validate(); err != nil {
		return err
	}
	a, _ := ParseAlgorithm(m.Algorithm)
	if info.Algorithm != a || info.Legacy != m.Legacy || info.Format != m.Format {
		return fmt.Errorf("%w: loaded image differs from manifest", ErrIncompatible)
	}
	if !m.Legacy && (info.ABI != m.ABI || info.Cores != m.Cores || info.Build != m.Build) {
		return fmt.Errorf("%w: loaded build/ABI/core count differs from manifest", ErrIncompatible)
	}
	return nil
}

// StageBitstream copies and hashes an artifact before any device is quiesced.
// Programming the private verified copy prevents replacement during the switch.
func StageBitstream(bitPath, checksum string) (string, func(), error) {
	in, err := os.Open(bitPath)
	if err != nil {
		return "", nil, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1024 || info.Size() > 128<<20 {
		return "", nil, errors.New("unexpected bitstream type or size")
	}
	dir, err := os.MkdirTemp("", "fpga-compress-image-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "image.bit")
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(in, (128<<20)+1))
	closeErr := out.Close()
	if copyErr != nil {
		cleanup()
		return "", nil, copyErr
	}
	if closeErr != nil {
		cleanup()
		return "", nil, closeErr
	}
	if hex.EncodeToString(hash.Sum(nil)) != checksum {
		cleanup()
		return "", nil, errors.New("bitstream SHA256 differs from manifest")
	}
	return path, cleanup, nil
}

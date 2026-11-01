package firmware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestManifestRejectsWrongIdentityAndChangedArtifact(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte{0x55}, 2048)
	path := filepath.Join(dir, "image.bit")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	m := Manifest{Schema: 1, Name: "test", Part: "xcku5p-ffvb676-2-e", Algorithm: "lz4", ABI: ABI, Format: 1, Cores: 4, Build: 42, Bitstream: "image.bit", SHA256: hex.EncodeToString(sum[:]), Validation: "timing-closed"}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	i, err := Inspect(context.Background(), goodRegisters(LZ4))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Check(i); err != nil {
		t.Fatal(err)
	}
	i.Build++
	if err := m.Check(i); err == nil {
		t.Fatal("accepted wrong build")
	}
	staged, cleanup, err := StageBitstream(path, m.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xaa}, 2048), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(staged)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("staged artifact changed", err)
	}
	if _, cleanup, err := StageBitstream(path, m.SHA256); err == nil {
		cleanup()
		t.Fatal("accepted modified bitstream")
	}
	m.Bitstream = "../image.bit"
	if err := m.Validate(); err == nil {
		t.Fatal("accepted escaping artifact")
	}
}

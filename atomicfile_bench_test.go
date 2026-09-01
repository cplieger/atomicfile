package atomicfile

import (
	"bytes"
	"fmt"
	"testing"
)

func BenchmarkWriteFile(b *testing.B) {
	sizes := []int{64, 4096, 64 * 1024, 1024 * 1024}
	for _, size := range sizes {
		data := bytes.Repeat([]byte("x"), size)
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			dir := b.TempDir()
			path := dir + "/target"
			ctx := b.Context()
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() { // excludes setup from timing; no ResetTimer needed
				if _, err := WriteFile(ctx, path, data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkWriteReader(b *testing.B) {
	sizes := []int{4096, 1024 * 1024}
	for _, size := range sizes {
		data := bytes.Repeat([]byte("y"), size)
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			dir := b.TempDir()
			path := dir + "/target"
			ctx := b.Context()
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := WriteReader(ctx, path, bytes.NewReader(data)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkPendingFileCommit(b *testing.B) {
	data := bytes.Repeat([]byte("z"), 4096)
	dir := b.TempDir()
	path := dir + "/target"
	ctx := b.Context()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		pf, err := NewPendingFile(ctx, path)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := pf.Write(data); err != nil {
			_ = pf.Cleanup()
			b.Fatal(err)
		}
		if _, err := pf.Commit(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMkdirChain separates the two WithMkdirMode paths: only the
// level-creating one pays for per-level mode enforcement and directory fsync.
func BenchmarkMkdirChain(b *testing.B) {
	b.Run("existing_parent", func(b *testing.B) {
		dir := b.TempDir()
		ctx := b.Context()
		path := dir + "/target"
		b.ReportAllocs()
		for b.Loop() {
			if _, err := WriteFile(ctx, path, []byte("x"), WithMkdirMode(0o755)); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("three_created_levels", func(b *testing.B) {
		base := b.TempDir()
		ctx := b.Context()
		n := 0
		b.ReportAllocs()
		for b.Loop() {
			n++
			// Fresh chain per iteration so each one creates three levels.
			path := fmt.Sprintf("%s/r%d/a/b/target", base, n)
			if _, err := WriteFile(ctx, path, []byte("x"), WithMkdirMode(0o755)); err != nil {
				b.Fatal(err)
			}
		}
	})
}

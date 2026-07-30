package atomicfile

// IsPackageTemp reports whether name is a temp file this package created:
// ".atomicfile-<digits>.tmp", with a non-empty all-digit middle. It is the
// exported face of the predicate both sweeps apply (CleanupStaleTemps and
// CleanupStaleTempsInRoot), so the agreement between a caller's file names and
// what the sweeps reclaim can be pinned in a test instead of trusted as prose.
//
// The all-digit requirement is the part callers cannot infer. A file that
// merely shares the prefix and suffix — ".atomicfile-notes.tmp" — is NOT a
// package temp and no sweep will ever delete it, which is exactly the property
// that makes the prefix safe to reuse for caller-owned files. Reconstructing
// the shape with os.CreateTemp(dir, ".atomicfile-*.tmp") happens to satisfy it
// today only because Go's implementation substitutes decimal digits for "*",
// which its documentation promises merely to be "a random string": use
// TempName instead of relying on that, and use this predicate to assert it.
//
// name is a single directory-entry name, not a path — the sweeps match on an
// entry's final element. Pass filepath.Base(path) if you hold a path.
func IsPackageTemp(name string) bool { return isStaleTempName(name) }

// TempName returns a fresh temp base name of the exact shape this package
// creates and IsPackageTemp recognises, drawn from crypto/rand.
//
// It exists so a caller that has to create a file of its own in a directory
// this package sweeps — a writability probe, a test fixture standing in for an
// interrupted write — gets a name the sweep reclaims without rebuilding a
// convention the package owns. The name is only a name: nothing is created, and
// two calls can be used for two files.
//
// A collision with an existing file is possible in principle (the middle is a
// 64-bit draw) and not handled here, because a caller creating the file with
// O_EXCL learns about it and can simply ask again — the way createTempInRoot
// does internally.
func TempName() string { return randomTempName() }

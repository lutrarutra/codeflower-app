package sandbox

// lockFile is not needed on Windows: Apptainer, the only engine that pulls to
// a file, does not run there.
func lockFile(string) (func(), error) {
	return func() {}, nil
}

// tryLockFile does not lock on Windows; the session's lease still keeps two
// runs of a task from continuing its session at once.
func tryLockFile(string) (func(), error) {
	return func() {}, nil
}

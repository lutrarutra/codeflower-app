package sandbox

// lockFile is not needed on Windows: Apptainer, the only engine that pulls to
// a file, does not run there.
func lockFile(string) (func(), error) {
	return func() {}, nil
}

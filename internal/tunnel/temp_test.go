package tunnel

import "os"

func mkShortTemp() (string, error) { return os.MkdirTemp("", "cft") }
func removeAll(path string)        { os.RemoveAll(path) }
